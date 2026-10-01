// spirula-reconstruct is the one-shot entry point for the pinned GPU image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/reconstruct"
)

type response struct {
	Status   string                    `json:"status"`
	Quality  reconstruct.QualitySignal `json:"quality"`
	Artifact *reconstruct.Artifact     `json:"artifact,omitempty"`
	Error    string                    `json:"error,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("spirula-reconstruct", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var request reconstruct.Request
	flags.StringVar(&request.InputPath, "input", "", "video file or directory of image frames")
	flags.StringVar(&request.OutputDir, "output", "", "new output directory for the final splat.ply")
	flags.Float64Var(&request.FrameFPS, "frame-fps", 0, "video extraction frame rate (default 12)")
	flags.StringVar(&request.SFMQuality, "sfm-quality", "", "SfM quality: low, medium, high, extreme")
	flags.StringVar(&request.TrainQuality, "train-quality", "", "training quality: low, medium, high, ultra")
	flags.IntVar(&request.MaxSplats, "cap-max", 0, "maximum splat count (default 1000000)")
	flags.IntVar(&request.Iterations, "iterations", 0, "training iterations (default 30000)")
	flags.IntVar(&request.TrainResolutionDivisor, "train-resolution-divisor", 0, "training input downscale divisor (default 1)")
	flags.Float64Var(&request.MinRegisteredPercent, "min-registered-percent", 0, "minimum SfM registered images percent (default 50)")
	flags.Float64Var(&request.MaxMeanReprojectionPX, "max-mean-reprojection-px", 0, "maximum SfM mean reprojection error (default 2)")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected positional arguments: %v\n", flags.Args())
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := (reconstruct.Runner{
		Executor:    reconstruct.NewOSExecutor(),
		FFmpegPath:  "/usr/bin/ffmpeg",
		SpirulaPath: "/usr/local/bin/spirula",
		Log:         os.Stderr,
	}).Run(ctx, request)
	status := "success"
	if err != nil {
		status = "failed"
	}
	payload := response{
		Status: status, Quality: result.Quality, Artifact: result.Artifact,
	}
	if err != nil {
		payload.Error = err.Error()
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(payload); encodeErr != nil {
		fmt.Fprintf(os.Stderr, "encode result: %v\n", encodeErr)
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}
