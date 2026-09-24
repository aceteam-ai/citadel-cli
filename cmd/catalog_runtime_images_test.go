package cmd

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

func TestPrePullRuntimeImagesDryRunHasNoNetworkEffect(t *testing.T) {
	images := testRuntimeImages()
	var output bytes.Buffer
	pullCalls := 0

	err := prePullRuntimeImages(context.Background(), images, "amd64", true, &output, func(context.Context, string) error {
		pullCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("prePullRuntimeImages: %v", err)
	}
	if pullCalls != 0 {
		t.Fatalf("pull called %d times during dry run", pullCalls)
	}
	for _, image := range images {
		if !strings.Contains(output.String(), image.Image) {
			t.Errorf("dry-run output missing %q: %s", image.Image, output.String())
		}
	}
}

func TestPrePullRuntimeImagesPullsInCatalogOrder(t *testing.T) {
	images := testRuntimeImages()
	var pulled []string

	err := prePullRuntimeImages(context.Background(), images, "amd64", false, &bytes.Buffer{}, func(_ context.Context, image string) error {
		pulled = append(pulled, image)
		return nil
	})
	if err != nil {
		t.Fatalf("prePullRuntimeImages: %v", err)
	}
	want := []string{images[0].Image, images[1].Image}
	if !reflect.DeepEqual(pulled, want) {
		t.Fatalf("pulled = %v, want %v", pulled, want)
	}
}

func TestPrePullRuntimeImagesStopsAndNamesFailedImage(t *testing.T) {
	images := testRuntimeImages()
	pullCalls := 0
	err := prePullRuntimeImages(context.Background(), images, "amd64", false, &bytes.Buffer{}, func(context.Context, string) error {
		pullCalls++
		return errors.New("registry unavailable")
	})
	if err == nil {
		t.Fatal("prePullRuntimeImages returned nil error")
	}
	if pullCalls != 1 {
		t.Fatalf("pull called %d times, want 1", pullCalls)
	}
	if !strings.Contains(err.Error(), images[0].Name) || !strings.Contains(err.Error(), images[0].Image) {
		t.Fatalf("error does not identify failed image: %v", err)
	}
}

func TestPrePullRuntimeImagesRejectsEmptyTrustedCatalog(t *testing.T) {
	err := prePullRuntimeImages(context.Background(), nil, "amd64", false, &bytes.Buffer{}, func(context.Context, string) error {
		t.Fatal("pull called with no images")
		return nil
	})
	if err == nil {
		t.Fatal("prePullRuntimeImages returned nil error")
	}
}

func TestPrePullRuntimeImagesPreflightsAllArchitecturesBeforePull(t *testing.T) {
	images := testRuntimeImages()
	images[1].Architectures = []string{"arm64"}
	pullCalls := 0

	err := prePullRuntimeImages(context.Background(), images, "amd64", false, &bytes.Buffer{}, func(context.Context, string) error {
		pullCalls++
		return nil
	})
	if err == nil {
		t.Fatal("prePullRuntimeImages returned nil error")
	}
	if pullCalls != 0 {
		t.Fatalf("pull called %d times before architecture preflight completed", pullCalls)
	}
	if !strings.Contains(err.Error(), `host architecture "amd64"`) || !strings.Contains(err.Error(), "node") {
		t.Fatalf("error does not identify host architecture and unsupported runtime: %v", err)
	}
}

func TestPrePullRuntimeImagesDryRunStillPreflightsArchitecture(t *testing.T) {
	images := testRuntimeImages()
	images[0].Architectures = []string{"arm64"}
	var output bytes.Buffer

	err := prePullRuntimeImages(context.Background(), images, "amd64", true, &output, func(context.Context, string) error {
		t.Fatal("pull called during dry run")
		return nil
	})
	if err == nil {
		t.Fatal("prePullRuntimeImages returned nil error")
	}
	if output.Len() != 0 {
		t.Fatalf("dry run printed partial plan before failed preflight: %q", output.String())
	}
}

func testRuntimeImages() []catalog.RuntimeImage {
	return []catalog.RuntimeImage{
		{Name: "python", Image: "ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DiscoveryAlias: "ghcr.io/aceteam-ai/aceteam-app-python:stable", Architectures: []string{"amd64", "arm64"}},
		{Name: "node", Image: "ghcr.io/aceteam-ai/aceteam-app-node@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", DiscoveryAlias: "ghcr.io/aceteam-ai/aceteam-app-node:stable", Architectures: []string{"amd64", "arm64"}},
	}
}
