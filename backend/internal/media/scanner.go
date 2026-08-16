package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ScanResult is the verdict on an object.
type ScanResult struct {
	Status string // clean, infected, skipped or failed
	Detail string
}

// Scanner checks uploaded bytes for malware (§72).
type Scanner interface {
	Scan(ctx context.Context, content io.Reader) (ScanResult, error)
	Enabled() bool
}

// NewScanner returns a ClamAV scanner when an address is configured, and a
// no-op scanner otherwise.
func NewScanner(address string) Scanner {
	if strings.TrimSpace(address) == "" {
		return disabledScanner{}
	}
	return &clamAVScanner{address: address}
}

type disabledScanner struct{}

func (disabledScanner) Enabled() bool { return false }

func (disabledScanner) Scan(context.Context, io.Reader) (ScanResult, error) {
	return ScanResult{Status: "skipped", Detail: "scanning is not configured"}, nil
}

// clamAVScanner streams content to clamd over its INSTREAM protocol.
//
// INSTREAM sends a sequence of length-prefixed chunks terminated by a
// zero-length chunk, which avoids clamd needing filesystem access to the
// object — important when the bytes live in object storage rather than on disk.
type clamAVScanner struct {
	address string
}

func (s *clamAVScanner) Enabled() bool { return true }

// chunkSize stays under clamd's default StreamMaxLength per-chunk expectations.
const clamChunkSize = 64 * 1024

func (s *clamAVScanner) Scan(ctx context.Context, content io.Reader) (ScanResult, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return ScanResult{Status: "failed"}, fmt.Errorf("media: dial clamav: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	}

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return ScanResult{Status: "failed"}, fmt.Errorf("media: send INSTREAM: %w", err)
	}

	buffer := make([]byte, clamChunkSize)
	sizePrefix := make([]byte, 4)
	for {
		n, readErr := content.Read(buffer)
		if n > 0 {
			binary.BigEndian.PutUint32(sizePrefix, uint32(n))
			if _, err := conn.Write(sizePrefix); err != nil {
				return ScanResult{Status: "failed"}, fmt.Errorf("media: send chunk size: %w", err)
			}
			if _, err := conn.Write(buffer[:n]); err != nil {
				return ScanResult{Status: "failed"}, fmt.Errorf("media: send chunk: %w", err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return ScanResult{Status: "failed"}, fmt.Errorf("media: read content: %w", readErr)
		}
	}

	// A zero-length chunk terminates the stream.
	binary.BigEndian.PutUint32(sizePrefix, 0)
	if _, err := conn.Write(sizePrefix); err != nil {
		return ScanResult{Status: "failed"}, fmt.Errorf("media: terminate stream: %w", err)
	}

	response, err := io.ReadAll(io.LimitReader(conn, 4096))
	if err != nil {
		return ScanResult{Status: "failed"}, fmt.Errorf("media: read clamav response: %w", err)
	}

	return parseClamAVResponse(string(response)), nil
}

// parseClamAVResponse interprets clamd's terse reply:
//
//	stream: OK
//	stream: Eicar-Test-Signature FOUND
//	stream: <reason> ERROR
func parseClamAVResponse(response string) ScanResult {
	trimmed := strings.TrimSpace(strings.TrimSuffix(response, "\x00"))

	switch {
	case strings.HasSuffix(trimmed, "OK"):
		return ScanResult{Status: "clean"}
	case strings.HasSuffix(trimmed, "FOUND"):
		signature := strings.TrimSpace(strings.TrimSuffix(trimmed, "FOUND"))
		signature = strings.TrimPrefix(signature, "stream:")
		return ScanResult{Status: "infected", Detail: strings.TrimSpace(signature)}
	case strings.HasSuffix(trimmed, "ERROR"):
		return ScanResult{Status: "failed", Detail: trimmed}
	default:
		// An unrecognised reply is treated as a failure rather than a pass:
		// "unknown" must never mean "clean".
		return ScanResult{Status: "failed", Detail: trimmed}
	}
}
