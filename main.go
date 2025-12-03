package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/blackjack/webcam"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
	"github.com/nfnt/resize"
	"golang.org/x/term"
)

var (
	col    = color.Color(color.RGBA{0, 0, 0, 0}) // if alpha is 0, use truecolor
	pixels = []rune{' ', '.', ',', ':', ';', 'i', '1', 't', 'f', 'L', 'C', 'G', '0', '8', '@'}
)

func main() {
	// graceful shutdown on SIGINT, SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\nShutting down...")
		cancel()
	}()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dev := flag.String("dev", "/dev/video0", "video device")
	sample := flag.String("sample", "bgsample", "Where to find/store the sample data")
	gen := flag.Bool("gen", false, "Generate a new background")
	screen := flag.Bool("greenscreen", false, "Use greenscreen")
	screenDist := flag.Float64("threshold", 0.13, "Greenscreen threshold")
	ansi := flag.Bool("ansi", false, "Use ANSI")
	usecol := flag.String("color", "", "Use single color")
	w := flag.Uint("width", 0, "output width")
	h := flag.Uint("height", 0, "output height")
	camWidth := flag.Uint("camWidth", 320, "cam input width")
	camHeight := flag.Uint("camHeight", 180, "cam input height")
	showFPS := flag.Bool("fps", false, "Show FPS")

	// GStreamer  flags
	gstMode := flag.Bool("gst", false, "Use GStreamer pipeline instead of /dev/videoX")
	gstPipeline := flag.String("gst-pipeline", "",
		"GStreamer pipeline that outputs raw RGB frames to fdsink fd=1")

	flag.Parse()

	if *usecol != "" {
		c, err := colorful.Hex(*usecol)
		if err != nil {
			return fmt.Errorf("invalid color: %v", err)
		}
		col = c
	}

	height := *h // height of the terminal output
	width := *w  // width of the terminal output

	// detect terminal width
	isTerminal := term.IsTerminal(int(os.Stdout.Fd()))
	if isTerminal {
		wTerm, hTerm, err := term.GetSize(int(os.Stdout.Fd()))
		if err == nil {
			if width == 0 {
				width = uint(wTerm)
			}
			if height == 0 {
				height = uint(hTerm)
			}
		}
	}
	if width == 0 {
		width = 125
	}
	if height == 0 {
		height = 50
	}

	// ANSI rendering uses half-height blocks
	if *ansi {
		height *= 2
	}

	var (
		cam       *webcam.Webcam
		err       error
		gstCmd    *exec.Cmd
		gstStdout io.ReadCloser
		gstReader *bufio.Reader
	)

	if *gstMode {
		if *gstPipeline == "" {
			return fmt.Errorf("-gst-pipeline is required when -gst is set")
		}

		gstCmd, gstStdout, err = startGstPipe(ctx, *gstPipeline)
		if err != nil {
			return fmt.Errorf("failed to start GStreamer pipeline: %w", err)
		}
		defer func() {
			_ = gstStdout.Close()
			if gstCmd.Process != nil {
				_ = gstCmd.Process.Kill()
			}
		}()
		gstReader = bufio.NewReader(gstStdout)
	} else {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(os.Stderr, "asciicam only works on Linux, use GStreamer mode instead")
			os.Exit(1)
		}
		cam, err = webcam.Open(*dev)
		if err != nil {
			return err
		}
		defer cam.Close()

		// find available yuyv format
		formats := cam.GetSupportedFormats()
		for k, v := range formats {
			fmt.Println(k, v)
			if strings.Contains(v, "YUYV") {
				f, wSet, hSet, err := cam.SetImageFormat(k, uint32(*camWidth), uint32(*camHeight))
				if err != nil {
					return fmt.Errorf("failed to set image format: %w", err)
				}
				fmt.Println(f, wSet, hSet)
				break
			}
		}

		// start streaming
		_ = cam.SetBufferCount(1)
		err = cam.StartStreaming()
		if err != nil {
			return fmt.Errorf("failed to start streaming: %w", err)
		}
		defer cam.StopStreaming()
	}

	var bg image.Image
	if !*gen && *screen {
		bg, err = loadBgSamples(*sample, width, height)
		if err != nil {
			return fmt.Errorf("could not load background samples: %w", err)
		}
	}

	p := termenv.EnvColorProfile()
	output := termenv.DefaultOutput()
	output.HideCursor()
	defer output.ShowCursor()
	output.AltScreen()
	defer output.ExitAltScreen()

	// seed fps counter
	var fps []float64
	for i := 0; i < 10; i++ {
		fps = append(fps, 0)
	}

	// buffer for gst RGB frames
	frameSizeRGB := int(*camWidth * *camHeight * 3)
	rgbBuf := make([]byte, frameSizeRGB)

	i := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		var img *image.RGBA

		if *gstMode {
			// Read exactly one RGB888 frame from GStreamer stdout
			if _, err := io.ReadFull(gstReader, rgbBuf); err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					fmt.Fprintln(os.Stderr, "GStreamer pipeline ended")
					return nil
				}
				return fmt.Errorf("failed to read from gst stdout: %w", err)
			}
			img = frameRGBToImage(rgbBuf, *camWidth, *camHeight)
		} else {
			// Webcam mode (YUYV)
			err = cam.WaitForFrame(1)
			switch err.(type) {
			case nil:
			case *webcam.Timeout:
				fmt.Fprintln(os.Stderr, err.Error())
				continue
			default:
				return fmt.Errorf("failed waiting for frame: %w", err)
			}

			frame, err := cam.ReadFrame()
			if err != nil {
				return fmt.Errorf("failed to read frame: %w", err)
			}
			if len(frame) == 0 {
				continue
			}
			img = frameToImage(frame, *camWidth, *camHeight)
		}

		// generate background sample data (still only really useful for webcam,
		// but works for gst as well if you want)
		if *gen {
			if err := os.MkdirAll(*sample, 0o755); err != nil {
				return fmt.Errorf("failed to create sample dir: %w", err)
			}
			f, err := os.Create(fmt.Sprintf("%s/%d.png", *sample, i))
			if err != nil {
				return fmt.Errorf("failed to create sample file: %w", err)
			}
			if err := png.Encode(f, img); err != nil {
				_ = f.Close()
				return fmt.Errorf("failed to encode sample frame: %w", err)
			}
			_ = f.Close()

			i++
			if i > 100 {
				os.Exit(0)
			}
		}

		// resize for further processing
		img = resize.Resize(width, height, img, resize.Bilinear).(*image.RGBA)

		// virtual green screen
		if !*gen && *screen {
			greenscreen(img, bg, *screenDist)
		}

		now := time.Now()

		// convert frame to ascii/ansi
		var s string
		if *ansi {
			s = imageToANSI(width, height, p, img)
		} else {
			s = imageToASCII(width, height, p, img)
		}

		// render
		output.MoveCursor(0, 0)
		fmt.Fprint(os.Stdout, s)

		if *showFPS {
			for i := len(fps) - 1; i > 0; i-- {
				fps[i] = fps[i-1]
			}
			fps[0] = float64(time.Second / time.Since(now))

			var fpsa float64
			for _, f := range fps {
				fpsa += f
			}

			fmt.Printf("FPS: %.0f", fpsa/float64(len(fps)))
		}
	}
}

// startGstPipe starts gst-launch-1.0 with the given pipeline and
// returns the *exec.Cmd and a ReadCloser for its stdout.
func startGstPipe(ctx context.Context, pipeline string) (*exec.Cmd, io.ReadCloser, error) {
	// split command: gst-launch-1.0 -e <elements...>
	args := append([]string{"-e"}, strings.Fields(pipeline)...)
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, nil, err
	}
	return cmd, stdout, nil
}

// Image helpers
func frameToImage(frame []byte, width, height uint) *image.RGBA {
	yuyv := image.NewYCbCr(image.Rect(0, 0, int(width), int(height)), image.YCbCrSubsampleRatio422)
	for i := range yuyv.Cb {
		ii := i * 4
		yuyv.Y[i*2] = frame[ii]
		yuyv.Y[i*2+1] = frame[ii+2]
		yuyv.Cb[i] = frame[ii+1]
		yuyv.Cr[i] = frame[ii+3]
	}

	b := yuyv.Bounds()
	img := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), yuyv, b.Min, draw.Src)

	return img
}

// frameRGBToImage converts a raw RGB888 frame (R,G,B bytes per pixel)
// into an *image.RGBA with the given width/height.
func frameRGBToImage(frame []byte, width, height uint) *image.RGBA {
	w := int(width)
	h := int(height)
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	stride := w * 3
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*stride + x*3
			if i+2 >= len(frame) {
				continue
			}
			r := frame[i]
			g := frame[i+1]
			b := frame[i+2]
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img
}

func pixelToASCII(pixel color.Color) rune {
	r2, g2, b2, a2 := pixel.RGBA()
	r := uint(r2 / 256)
	g := uint(g2 / 256)
	b := uint(b2 / 256)
	a := uint(a2 / 256)

	intensity := (r + g + b) * a / 255
	precision := float64(255 * 3 / (len(pixels) - 1))

	v := int(math.Floor(float64(intensity)/precision + 0.5))
	return pixels[v]
}

func imageToASCII(width, height uint, p termenv.Profile, img image.Image) string {
	str := strings.Builder{}

	for i := 0; i < int(height); i++ {
		for j := 0; j < int(width); j++ {
			pixel := color.NRGBAModel.Convert(img.At(j, i))
			s := termenv.String(string(pixelToASCII(pixel)))

			_, _, _, a := col.RGBA()
			if a > 0 {
				s = s.Foreground(p.FromColor(col))
			} else {
				s = s.Foreground(p.FromColor(pixel))
			}
			str.WriteString(s.String())
		}
		str.WriteString("\n")
	}

	return str.String()
}

func imageToANSI(_, _ uint, p termenv.Profile, img image.Image) string {
	b := img.Bounds()

	str := strings.Builder{}
	for y := 0; y < b.Max.Y; y += 2 {
		for x := 0; x < b.Max.X; x++ {
			str.WriteString(termenv.String("▀").
				Foreground(p.FromColor(img.At(x, y))).
				Background(p.FromColor(img.At(x, y+1))).
				String())
		}
		str.WriteString("\n")
	}

	return str.String()
}

func greenscreen(img *image.RGBA, bg image.Image, dist float64) {
	if bg == nil {
		return
	}

	for y := 0; y < img.Bounds().Size().Y; y++ {
		for x := 0; x < img.Bounds().Size().X; x++ {
			c1, _ := colorful.MakeColor(img.At(x, y))
			c2, _ := colorful.MakeColor(bg.At(x, y))

			if c1.DistanceLab(c2) < dist {
				img.Set(x, y, image.Transparent)
			}
		}
	}
}

func loadBgSamples(path string, width, height uint) (image.Image, error) {
	i := 40
	b, err := os.ReadFile(fmt.Sprintf("%s/%d.png", path, i))
	if err != nil {
		return nil, err
	}

	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	return resize.Resize(width, height, img, resize.Bilinear).(*image.RGBA), nil
}
