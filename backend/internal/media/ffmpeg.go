package media

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ffmpeg and ffprobe do the video and audio work (§21, §22). They are external
// binaries rather than a cgo library so the API image stays distroless: only
// the worker image needs them installed.
const (
	ffmpegBinary  = "ffmpeg"
	ffprobeBinary = "ffprobe"
	probeTimeout  = 30 * time.Second
	encodeTimeout = 30 * time.Minute
)

// FFmpegAvailable reports whether the toolchain is installed. Callers degrade
// to storing the original when it is not, rather than failing the upload.
func FFmpegAvailable() bool {
	_, ffmpegErr := exec.LookPath(ffmpegBinary)
	_, ffprobeErr := exec.LookPath(ffprobeBinary)
	return ffmpegErr == nil && ffprobeErr == nil
}

// MediaInfo is what ffprobe reports about a file.
type MediaInfo struct {
	Width      int
	Height     int
	DurationMs int
	Bitrate    int
	VideoCodec string
	AudioCodec string
	HasAudio   bool
	HasVideo   bool
}

// Probe inspects a local file.
func Probe(ctx context.Context, path string) (*MediaInfo, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, ffprobeBinary,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("media: ffprobe: %w", err)
	}

	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Duration  string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, fmt.Errorf("media: parse ffprobe output: %w", err)
	}

	info := &MediaInfo{}
	for _, stream := range probe.Streams {
		switch stream.CodecType {
		case "video":
			// Cover art inside an audio file also reports as a video stream;
			// only a stream with real dimensions counts.
			if stream.Width > 0 && stream.Height > 0 && !info.HasVideo {
				info.HasVideo = true
				info.Width = stream.Width
				info.Height = stream.Height
				info.VideoCodec = stream.CodecName
			}
		case "audio":
			info.HasAudio = true
			info.AudioCodec = stream.CodecName
		}
	}

	if seconds, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
		info.DurationMs = int(seconds * 1000)
	}
	if bitrate, err := strconv.Atoi(probe.Format.BitRate); err == nil {
		info.Bitrate = bitrate / 1000
	}
	return info, nil
}

// VideoRendition describes one transcoding target (§21).
type VideoRendition struct {
	Name    string
	Height  int
	Bitrate string
	Audio   string
}

// videoRenditions are produced only when the source is at least as tall, so a
// 480p upload never gets upscaled into a larger, worse-looking file.
var videoRenditions = []VideoRendition{
	{Name: "360p", Height: 360, Bitrate: "800k", Audio: "96k"},
	{Name: "720p", Height: 720, Bitrate: "2500k", Audio: "128k"},
	{Name: "1080p", Height: 1080, Bitrate: "5000k", Audio: "192k"},
}

// TranscodeVideo produces an H.264/AAC MP4 at the requested height.
func TranscodeVideo(ctx context.Context, sourcePath, targetPath string, rendition VideoRendition) error {
	encodeCtx, cancel := context.WithTimeout(ctx, encodeTimeout)
	defer cancel()

	cmd := exec.CommandContext(encodeCtx, ffmpegBinary,
		"-y", "-i", sourcePath,
		// Keep the aspect ratio and force even dimensions, which H.264 requires.
		"-vf", fmt.Sprintf("scale=-2:%d", rendition.Height),
		"-c:v", "libx264", "-preset", "medium", "-crf", "23",
		"-maxrate", rendition.Bitrate, "-bufsize", rendition.Bitrate,
		"-c:a", "aac", "-b:a", rendition.Audio,
		// Move the index to the front so playback can start before the whole
		// file has been fetched.
		"-movflags", "+faststart",
		targetPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("media: transcode %s: %w: %s", rendition.Name, err, tail(output, 400))
	}
	return nil
}

// ExtractPoster grabs a representative frame for the video thumbnail.
func ExtractPoster(ctx context.Context, sourcePath, targetPath string, atMs int) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	timestamp := time.Duration(atMs) * time.Millisecond
	cmd := exec.CommandContext(probeCtx, ffmpegBinary,
		"-y", "-ss", formatTimestamp(timestamp), "-i", sourcePath,
		"-frames:v", "1", "-q:v", "3", targetPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("media: extract poster: %w: %s", err, tail(output, 400))
	}
	return nil
}

// TranscodeVoice normalises a voice message to Opus in an Ogg container (§22).
func TranscodeVoice(ctx context.Context, sourcePath, targetPath string) error {
	encodeCtx, cancel := context.WithTimeout(ctx, encodeTimeout)
	defer cancel()

	cmd := exec.CommandContext(encodeCtx, ffmpegBinary,
		"-y", "-i", sourcePath,
		"-c:a", "libopus", "-b:a", "32k",
		// Voice messages are mono at 48 kHz: anything more is wasted bytes.
		"-ac", "1", "-ar", "48000",
		"-application", "voip",
		targetPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("media: transcode voice: %w: %s", err, tail(output, 400))
	}
	return nil
}

// ExtractWaveform produces the amplitude peaks the client draws under a voice
// message. `buckets` peaks are returned, each scaled to 0–100.
func ExtractWaveform(ctx context.Context, sourcePath string, buckets int) ([]int16, error) {
	if buckets <= 0 {
		buckets = 64
	}

	encodeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// Decode to raw mono 16-bit PCM at a low sample rate: the waveform only
	// needs an envelope, not fidelity.
	cmd := exec.CommandContext(encodeCtx, ffmpegBinary,
		"-v", "error", "-i", sourcePath,
		"-ac", "1", "-ar", "8000",
		"-f", "s16le", "-",
	)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("media: decode for waveform: %w", err)
	}

	samples := len(raw) / 2
	if samples == 0 {
		return nil, nil
	}
	if samples < buckets {
		buckets = samples
	}

	peaks := make([]int16, buckets)
	samplesPerBucket := samples / buckets
	if samplesPerBucket == 0 {
		samplesPerBucket = 1
	}

	globalPeak := 0.0
	rawPeaks := make([]float64, buckets)

	for bucket := 0; bucket < buckets; bucket++ {
		start := bucket * samplesPerBucket
		end := start + samplesPerBucket
		if end > samples {
			end = samples
		}

		// Root mean square tracks perceived loudness better than a raw
		// maximum, which any single click would dominate.
		sum := 0.0
		for i := start; i < end; i++ {
			sample := float64(int16(uint16(raw[i*2]) | uint16(raw[i*2+1])<<8))
			sum += sample * sample
		}
		if end > start {
			rms := math.Sqrt(sum / float64(end-start))
			rawPeaks[bucket] = rms
			globalPeak = math.Max(globalPeak, rms)
		}
	}

	if globalPeak == 0 {
		return peaks, nil
	}
	// Normalise so a quiet recording still renders a visible waveform.
	for i, peak := range rawPeaks {
		peaks[i] = int16(math.Round(peak / globalPeak * 100))
	}
	return peaks, nil
}

// RenditionsFor selects the renditions worth producing for a source height.
func RenditionsFor(sourceHeight int) []VideoRendition {
	var out []VideoRendition
	for _, rendition := range videoRenditions {
		if sourceHeight >= rendition.Height {
			out = append(out, rendition)
		}
	}
	if len(out) == 0 && len(videoRenditions) > 0 {
		// A source smaller than every target still gets the lowest rendition,
		// so playback has a consistent format to fall back on.
		out = append(out, videoRenditions[0])
	}
	return out
}

// TempFile creates a working file in a directory that is removed with cleanup.
func TempFile(dir, pattern string) (string, func(), error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", nil, fmt.Errorf("media: create temp file: %w", err)
	}
	path := file.Name()
	_ = file.Close()
	return path, func() { _ = os.Remove(path) }, nil
}

// TempDir creates a scratch directory for one processing job.
func TempDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "sobh-media-*")
	if err != nil {
		return "", nil, fmt.Errorf("media: create temp dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func formatTimestamp(d time.Duration) string {
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// tail returns the last n bytes of command output, which is where ffmpeg puts
// the actual error.
func tail(output []byte, n int) string {
	text := strings.TrimSpace(string(output))
	if len(text) <= n {
		return text
	}
	return "…" + text[len(text)-n:]
}

// VariantKey derives the storage key for a derived rendition, keeping it beside
// the original.
func VariantKey(originalKey, variant, extension string) string {
	dir := filepath.Dir(originalKey)
	base := strings.TrimSuffix(filepath.Base(originalKey), filepath.Ext(originalKey))
	return filepath.Join(dir, fmt.Sprintf("%s_%s%s", base, variant, extension))
}
