package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"

	// Registered for their decoders only; the pipeline always re-encodes.
	_ "image/gif"
)

// Variant names and their longest-edge targets (§20).
//
// Every variant is a bounded box, not a fixed size: the aspect ratio is always
// preserved, so a panorama and a portrait both stay recognisable.
var imageVariants = []struct {
	name    string
	maxEdge int
	quality int
}{
	{"thumbnail", 160, 70},
	{"small", 320, 75},
	{"medium", 800, 80},
	{"preview", 1600, 85},
}

// ImageVariant is one produced rendition, ready to be stored.
type ImageVariant struct {
	Name     string
	Data     []byte
	MimeType string
	Width    int
	Height   int
}

// ProcessImage decodes an image and produces the variant set plus a BlurHash
// placeholder. Variants larger than the source are skipped: upscaling adds
// bytes without adding detail.
func ProcessImage(data []byte) (variants []ImageVariant, width, height int, blurhash string, err error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("media: decode image: %w", err)
	}

	bounds := source.Bounds()
	width, height = bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return nil, 0, 0, "", fmt.Errorf("media: image has zero dimensions")
	}

	longestEdge := width
	if height > longestEdge {
		longestEdge = height
	}

	for _, spec := range imageVariants {
		if spec.maxEdge > longestEdge && spec.name != "thumbnail" {
			continue
		}

		resized := resizeToBox(source, spec.maxEdge)
		encoded, encodeErr := encodeJPEG(resized, spec.quality)
		if encodeErr != nil {
			return nil, 0, 0, "", encodeErr
		}
		variants = append(variants, ImageVariant{
			Name:     spec.name,
			Data:     encoded,
			MimeType: "image/jpeg",
			Width:    resized.Bounds().Dx(),
			Height:   resized.Bounds().Dy(),
		})
	}

	blurhash = EncodeBlurHash(resizeToBox(source, 64), 4, 3)
	return variants, width, height, blurhash, nil
}

// resizeToBox scales an image so its longest edge is at most maxEdge.
func resizeToBox(source image.Image, maxEdge int) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	scale := float64(maxEdge) / float64(max(width, height))
	if scale > 1 {
		scale = 1
	}

	targetWidth := max(1, int(math.Round(float64(width)*scale)))
	targetHeight := max(1, int(math.Round(float64(height)*scale)))

	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	// Transparent source pixels would otherwise turn black once flattened to
	// JPEG, so the canvas starts white.
	draw.Draw(target, target.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	// CatmullRom is the best quality/cost trade-off for downscaling photos.
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, bounds, xdraw.Over, nil)
	return target
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("media: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// EncodePNG is used for stickers, where transparency must survive.
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("media: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------- BlurHash
//
// A compact placeholder the client renders while the real image loads. The
// implementation follows the reference BlurHash specification: the image is
// projected onto a small cosine basis and the coefficients are packed into a
// short base-83 string.

const blurHashAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~"

// EncodeBlurHash produces a hash with componentsX x componentsY components,
// each between 1 and 9.
func EncodeBlurHash(img image.Image, componentsX, componentsY int) string {
	componentsX = clamp(componentsX, 1, 9)
	componentsY = clamp(componentsY, 1, 9)

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return ""
	}

	factors := make([][3]float64, 0, componentsX*componentsY)
	for y := 0; y < componentsY; y++ {
		for x := 0; x < componentsX; x++ {
			factors = append(factors, multiplyBasisFunction(img, bounds, x, y))
		}
	}

	direct := factors[0]
	acComponents := factors[1:]

	var hash []byte
	sizeFlag := (componentsX - 1) + (componentsY-1)*9
	hash = appendBase83(hash, sizeFlag, 1)

	maximumValue := 0.0
	if len(acComponents) > 0 {
		actualMax := 0.0
		for _, factor := range acComponents {
			for _, channel := range factor {
				actualMax = math.Max(actualMax, math.Abs(channel))
			}
		}
		quantisedMax := clamp(int(math.Floor(actualMax*166-0.5)), 0, 82)
		maximumValue = (float64(quantisedMax) + 1) / 166
		hash = appendBase83(hash, quantisedMax, 1)
	} else {
		maximumValue = 1
		hash = appendBase83(hash, 0, 1)
	}

	hash = appendBase83(hash, encodeDC(direct), 4)
	for _, factor := range acComponents {
		hash = appendBase83(hash, encodeAC(factor, maximumValue), 2)
	}
	return string(hash)
}

func multiplyBasisFunction(img image.Image, bounds image.Rectangle, componentX, componentY int) [3]float64 {
	var r, g, b float64
	width, height := bounds.Dx(), bounds.Dy()

	normalisation := 2.0
	if componentX == 0 && componentY == 0 {
		normalisation = 1.0
	}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			basis := math.Cos(math.Pi*float64(componentX)*float64(x)/float64(width)) *
				math.Cos(math.Pi*float64(componentY)*float64(y)/float64(height))

			pixelR, pixelG, pixelB, _ := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			r += basis * sRGBToLinear(int(pixelR>>8))
			g += basis * sRGBToLinear(int(pixelG>>8))
			b += basis * sRGBToLinear(int(pixelB>>8))
		}
	}

	scale := normalisation / float64(width*height)
	return [3]float64{r * scale, g * scale, b * scale}
}

func encodeDC(value [3]float64) int {
	return linearToSRGB(value[0])<<16 + linearToSRGB(value[1])<<8 + linearToSRGB(value[2])
}

func encodeAC(value [3]float64, maximumValue float64) int {
	quantise := func(channel float64) int {
		return clamp(int(math.Floor(signPow(channel/maximumValue, 0.5)*9+9.5)), 0, 18)
	}
	return quantise(value[0])*19*19 + quantise(value[1])*19 + quantise(value[2])
}

func appendBase83(dst []byte, value, length int) []byte {
	for i := 1; i <= length; i++ {
		digit := (value / int(math.Pow(83, float64(length-i)))) % 83
		dst = append(dst, blurHashAlphabet[digit])
	}
	return dst
}

func sRGBToLinear(value int) float64 {
	v := float64(value) / 255
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func linearToSRGB(value float64) int {
	v := math.Max(0, math.Min(1, value))
	if v <= 0.0031308 {
		return int(v*12.92*255 + 0.5)
	}
	return int((1.055*math.Pow(v, 1/2.4)-0.055)*255 + 0.5)
}

func signPow(value, exponent float64) float64 {
	return math.Copysign(math.Pow(math.Abs(value), exponent), value)
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
