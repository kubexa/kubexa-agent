package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Until this program existed, the image claimed in EULA.md section 2 that the
// notices of its compiled-in third-party libraries were "reproduced in the
// Software's distribution". No Dockerfile copied a licence file and no repo
// held one, so MIT, BSD and Apache-2.0 attribution travelled with nothing.
// These tests pin the three properties that make the generated file worth
// shipping: it finds a licence whatever the upstream called the file, it
// reproduces the text verbatim rather than naming the licence, and a module
// whose licence it cannot find is an ERROR rather than a silent omission.

func TestLicenseFileFindsEveryUpstreamSpelling(t *testing.T) {
	for _, name := range []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "LICENCE", "LICENCE.md", "LICENSE-MIT"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("text of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := licenseFile(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if filepath.Base(got) != name {
			t.Errorf("%s: found %q", name, got)
		}
	}
}

func TestLicenseFileReportsAbsenceWithoutGuessing(t *testing.T) {
	// A directory with source but no licence must not fall back to the README
	// or to a licence name inferred from the module path.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("MIT licensed!"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := licenseFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("found %q in a directory with no licence file", got)
	}
}

func TestCollectSeparatesOurOwnModulesFromThirdParty(t *testing.T) {
	// The agent module carries GPL-3.0 and the same copyright as the Software.
	// Listing it among the third-party notices would read as a GPL obligation
	// on a proprietary image, which is the confusion the README exists to end.
	ours, theirs := t.TempDir(), t.TempDir()
	write(t, ours, "LICENSE", "GPL-3.0 text")
	write(t, theirs, "LICENSE", "MIT text")

	got, err := collect([]module{
		{Path: "github.com/kubexa/kubexa-agent", Version: "v0.11.0", Dir: ours},
		{Path: "github.com/gofiber/fiber/v2", Version: "v2.52.0", Dir: theirs},
	}, "github.com/kubexa/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.own) != 1 || got.own[0].Path != "github.com/kubexa/kubexa-agent" {
		t.Errorf("own = %+v", got.own)
	}
	if len(got.thirdParty) != 1 || got.thirdParty[0].Path != "github.com/gofiber/fiber/v2" {
		t.Errorf("thirdParty = %+v", got.thirdParty)
	}
}

func TestCollectFailsOnAModuleWithNoLicence(t *testing.T) {
	// The whole point is that a gap is loud. A module we cannot attribute has
	// to break the build, not produce a shorter file.
	bare := t.TempDir()
	_, err := collect([]module{{Path: "example.com/bare", Version: "v1.0.0", Dir: bare}}, "github.com/kubexa/")
	if err == nil {
		t.Fatal("collect accepted a module with no licence file")
	}
	if !strings.Contains(err.Error(), "example.com/bare") {
		t.Errorf("error does not name the module: %v", err)
	}
}

func TestDocumentReproducesTheLicenceTextVerbatim(t *testing.T) {
	// Naming a licence is not reproducing it: MIT and BSD both require the
	// copyright line itself to travel with the binary.
	const text = "MIT License\n\nCopyright (c) 2019 Someone Else\n\nPermission is hereby granted..."
	doc := document(collected{
		thirdParty: []entry{{Path: "example.com/x", Version: "v1.2.3", Text: text}},
		own:        []entry{{Path: "github.com/kubexa/kubexa-agent", Version: "v0.11.0", Text: "GPL-3.0 text"}},
	})

	if !strings.Contains(doc, text) {
		t.Error("licence text is not reproduced verbatim")
	}
	if !strings.Contains(doc, "example.com/x") || !strings.Contains(doc, "v1.2.3") {
		t.Error("module path and version are not both stated")
	}
	// The own-code section must say why it is listed separately, or a reader
	// doing a licence scan reads GPL-3.0 as an obligation on the image.
	if !strings.Contains(doc, "github.com/kubexa/kubexa-agent") {
		t.Error("own modules are not listed at all")
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
