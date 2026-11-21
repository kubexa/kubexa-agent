// Command licensenotices writes the third-party licence notices that the
// images are required to carry.
//
// EULA.md section 2 tells a customer that the notices of the libraries compiled
// into the Software are "reproduced in the Software's distribution". MIT, BSD
// and Apache-2.0 each require exactly that of a binary redistribution, and
// until this program ran no image carried a single licence file. It is run in
// the Docker builder stage, where the module cache is already populated, and
// its output is copied into the runtime image.
//
// It fails rather than omitting: a module whose licence file cannot be found
// stops the build. A silently shorter notices file is the failure this program
// exists to prevent, so it is not an outcome the program is allowed to produce.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// module is one entry of `go list -deps` output: a dependency that is linked
// into the binaries, not merely present in the module graph. The two differ by
// a wide margin, so the package set passed on the command line matters.
type module struct {
	Path    string
	Version string
	Dir     string
}

type entry struct {
	Path    string
	Version string
	Text    string
}

type collected struct {
	thirdParty []entry
	own        []entry
}

// licenseFileNames is ordered, so a module shipping both LICENSE and
// LICENSE-MIT is reported under the conventional name. The British spellings
// and the -MIT/-APACHE suffixes are not hypothetical: aidanwoods.dev/go-paseto
// ships LICENCE.md and nothing else.
var licenseFileNames = []string{
	"LICENSE", "LICENSE.txt", "LICENSE.md",
	"LICENCE", "LICENCE.txt", "LICENCE.md",
	"COPYING", "COPYING.txt", "COPYING.md",
	"LICENSE-MIT", "LICENSE-APACHE", "LICENSE-BSD",
}

// licenseFile returns the licence file in dir, or "" when the module ships
// none. It never infers one: a README that mentions MIT is not a licence, and
// guessing would reproduce a notice the copyright holder did not write.
func licenseFile(dir string) (string, error) {
	for _, name := range licenseFileNames {
		p := filepath.Join(dir, name)
		switch _, err := os.Stat(p); {
		case err == nil:
			return p, nil
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return "", fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return "", nil
}

// collect reads every module's licence text. Modules under ownPrefix are kept
// apart: the agent module carries GPL-3.0 and the same copyright as the
// Software itself, and listing it among the third-party notices would read to
// a licence scanner as a GPL obligation on a proprietary image.
func collect(mods []module, ownPrefix string) (collected, error) {
	var out collected
	var missing []string

	for _, m := range mods {
		f, err := licenseFile(m.Dir)
		if err != nil {
			return collected{}, err
		}
		if f == "" {
			missing = append(missing, m.Path+"@"+m.Version)
			continue
		}
		text, err := os.ReadFile(f)
		if err != nil {
			return collected{}, fmt.Errorf("read %s: %w", f, err)
		}
		e := entry{Path: m.Path, Version: m.Version, Text: string(text)}
		if ownPrefix != "" && strings.HasPrefix(m.Path, ownPrefix) {
			out.own = append(out.own, e)
			continue
		}
		out.thirdParty = append(out.thirdParty, e)
	}

	if len(missing) > 0 {
		return collected{}, fmt.Errorf(
			"no licence file found for %d module(s), so they cannot be attributed: %s",
			len(missing), strings.Join(missing, ", "))
	}
	return out, nil
}

func document(c collected) string {
	var b strings.Builder

	b.WriteString("# Third-party notices\n\n")
	b.WriteString("This file is generated at build time from the modules actually linked\n")
	b.WriteString("into this image's binaries, and it reproduces each one's licence in\n")
	b.WriteString("full. It is not a summary: MIT and BSD both require the copyright line\n")
	b.WriteString("itself to travel with the binary.\n\n")
	// Fprintf, not WriteString(Sprintf(...)): staticcheck QF1012 is an error
	// in the agent repo's golangci-lint config and failed that build.
	fmt.Fprintf(&b, "%d third-party modules are listed.\n\n", len(c.thirdParty))

	if len(c.own) > 0 {
		b.WriteString("## Kubexa's own modules\n\n")
		b.WriteString("These are listed because a licence scanner will find them in the\n")
		b.WriteString("binary's module list, and their presence raises a question worth\n")
		b.WriteString("answering directly. They carry the same copyright as this image. A\n")
		b.WriteString("copyright holder needs no licence from itself, so the terms below\n")
		b.WriteString("bind recipients of those modules and place no obligation on this\n")
		b.WriteString("image. See the agent repository's README for the reasoning.\n\n")
		for _, e := range c.own {
			b.WriteString(section(e))
		}
	}

	b.WriteString("## Third-party modules\n\n")
	for _, e := range c.thirdParty {
		b.WriteString(section(e))
	}
	return b.String()
}

func section(e entry) string {
	var b strings.Builder
	b.WriteString("### " + e.Path + "\n\n")
	b.WriteString("Version: " + e.Version + "\n\n")
	b.WriteString("```\n")
	// A licence text containing a fence would end the block early; none of the
	// licences in use do, but a future dependency is not ours to vouch for.
	b.WriteString(strings.ReplaceAll(e.Text, "```", "'''"))
	if !strings.HasSuffix(e.Text, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n\n")
	return b.String()
}

// listModules asks the toolchain which modules are linked into pkgs. The
// module directory comes from `go list` rather than being derived from the
// module path: the cache escapes capital letters, and reconstructing that by
// hand is how an earlier attempt at this silently found zero licences.
func listModules(pkgs []string) ([]module, error) {
	args := append([]string{"list", "-deps", "-f",
		"{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}"}, pkgs...)
	cmd := exec.Command("go", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}

	seen := map[string]bool{}
	var mods []module
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 || fields[0] == "" {
			continue // the standard library has no module
		}
		path, version, dir := fields[0], fields[1], fields[2]
		// The main module has no version and needs no notice: it is the thing
		// being licensed, not a dependency of it.
		if version == "" || dir == "" || seen[path] {
			continue
		}
		seen[path] = true
		mods = append(mods, module{Path: path, Version: version, Dir: dir})
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}

func main() {
	out := flag.String("o", "THIRD-PARTY-NOTICES.md", "file to write")
	own := flag.String("own", "github.com/kubexa/", "module prefix that shares this image's copyright")
	flag.Parse()

	pkgs := flag.Args()
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}

	mods, err := listModules(pkgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensenotices:", err)
		os.Exit(1)
	}
	c, err := collect(mods, *own)
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensenotices:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(document(c)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "licensenotices:", err)
		os.Exit(1)
	}
	fmt.Printf("licensenotices: %d third-party + %d own module(s) -> %s\n",
		len(c.thirdParty), len(c.own), *out)
}
