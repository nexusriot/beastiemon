// Command mkpkg writes a FreeBSD pkg(8) package from a staged install tree.
//
// It exists so a Linux or macOS workstation can cut a release without a
// FreeBSD host: scripts/mkpkg.sh prefers the native pkg(8) when it is around
// and falls back to this. The two produce packages pkg(8) installs
// identically; this one carries no per-file checksums (the manifest says "-",
// which pkg accepts) and no +COMPACT_MANIFEST, which only repository
// catalogues read.
//
// The layout is what `pkg create` emits and what pkg(8) expects:
//
//	+MANIFEST                       (first member)
//	/usr/local/bin/beastied         (absolute member names, files only —
//	/usr/local/bin/beastie           directory entries make pkg warn that
//	...                              they are "not specified in the manifest")
//
// Usage:
//
//	go run ./scripts/mkpkg -stage .stage -manifest freebsd/+MANIFEST \
//	    -version 0.2.0 -abi FreeBSD:15:amd64 -arch freebsd:15:x86:64 \
//	    -out .pkg/beastiemon-0.2.0.pkg
package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("mkpkg: ")

	stage := flag.String("stage", ".stage", "staged install tree")
	manifest := flag.String("manifest", "freebsd/+MANIFEST", "manifest template")
	version := flag.String("version", "", "package version, substituted for %%VERSION%%")
	abi := flag.String("abi", "FreeBSD:15:amd64", "pkg ABI the package declares")
	arch := flag.String("arch", "", "pkg arch (derived from -abi when empty)")
	out := flag.String("out", "", "output .pkg path")
	flag.Parse()

	if *version == "" || *out == "" {
		flag.Usage()
		log.Fatal("-version and -out are required")
	}
	if *arch == "" {
		a, err := archFromABI(*abi)
		if err != nil {
			log.Fatal(err)
		}
		*arch = a
	}

	files, total, err := collect(*stage)
	if err != nil {
		log.Fatal(err)
	}
	if len(files) == 0 {
		log.Fatalf("%s holds no files — run scripts/stage.sh first", *stage)
	}

	body, err := os.ReadFile(*manifest)
	if err != nil {
		log.Fatal(err)
	}
	text := strings.ReplaceAll(string(body), "%%VERSION%%", *version)

	// The manifest's files{} list is what pkg installs and later verifies, so
	// a file staged but not listed would be silently dropped on install and a
	// file listed but not staged would break it. Catching the mismatch here
	// turns a packaging slip into a build failure.
	if err := checkManifestFiles(text, files); err != nil {
		log.Fatal(err)
	}

	text += fmt.Sprintf("\nabi: %q\narch: %q\nflatsize: %d\n", *abi, *arch, total)

	if err := write(*out, *stage, text, files); err != nil {
		log.Fatal(err)
	}

	info, err := os.Stat(*out)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%d files, %d bytes installed, %d bytes packaged, abi %s)\n",
		*out, len(files), total, info.Size(), *abi)
}

// archFromABI turns "FreeBSD:15:amd64" into pkg's lowercase arch spelling,
// "freebsd:15:x86:64". pkg records both and they have to agree.
func archFromABI(abi string) (string, error) {
	parts := strings.Split(abi, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("abi %q is not OS:version:arch", abi)
	}
	machine, ok := map[string]string{
		"amd64":   "x86:64",
		"i386":    "x86:32",
		"aarch64": "aarch64:64",
		"arm64":   "aarch64:64",
		"powerpc": "powerpc:32",
	}[parts[2]]
	if !ok {
		return "", fmt.Errorf("abi %q: unknown architecture %q", abi, parts[2])
	}
	return strings.ToLower(parts[0]) + ":" + parts[1] + ":" + machine, nil
}

// staged is one file from the stage, carrying the install path pkg will use.
type staged struct {
	install string // absolute path on the target, e.g. /usr/local/bin/beastied
	source  string // where it currently lives
	mode    fs.FileMode
	size    int64
}

// collect walks the stage, skipping the +MANIFEST/+DESC scratch files a
// previous `pkg create` run may have left behind.
func collect(stage string) ([]staged, int64, error) {
	var files []staged
	var total int64
	err := filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "+") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, staged{
			install: "/" + filepath.ToSlash(rel),
			source:  p,
			mode:    info.Mode().Perm(),
			size:    info.Size(),
		})
		total += info.Size()
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].install < files[j].install })
	return files, total, err
}

// manifestPath pulls the install paths out of the manifest's files{} block.
// The template is UCL, so this reads the quoted keys rather than parsing it.
var manifestPath = regexp.MustCompile(`(?m)^\s*(/[^\s:"]+):`)

func checkManifestFiles(manifest string, files []staged) error {
	start := strings.Index(manifest, "files:")
	if start < 0 {
		return fmt.Errorf("manifest has no files: block")
	}
	end := strings.Index(manifest[start:], "\n}")
	if end < 0 {
		return fmt.Errorf("manifest files: block is not closed")
	}
	listed := map[string]bool{}
	for _, m := range manifestPath.FindAllStringSubmatch(manifest[start:start+end], -1) {
		listed[m[1]] = true
	}
	staged := map[string]bool{}
	for _, f := range files {
		staged[f.install] = true
	}

	var missing, extra []string
	for p := range listed {
		if !staged[p] {
			missing = append(missing, p)
		}
	}
	for p := range staged {
		if !listed[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		return fmt.Errorf("manifest lists files the stage does not have: %s",
			strings.Join(missing, " "))
	}
	if len(extra) > 0 {
		return fmt.Errorf("stage holds files the manifest does not list "+
			"(pkg would not install them): %s", strings.Join(extra, " "))
	}
	return nil
}

func write(out, stage, manifest string, files []staged) error {
	if dir := filepath.Dir(out); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := addFile(tw, "+MANIFEST", 0o644, []byte(manifest)); err != nil {
		return err
	}
	for _, s := range files {
		body, err := os.ReadFile(s.source)
		if err != nil {
			return err
		}
		if err := addFile(tw, s.install, s.mode, body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

// addFile writes one member. Ownership is root:wheel because that is what the
// files become on the target, and the timestamp is left at the zero value so
// two runs over the same tree produce identical bytes.
func addFile(tw *tar.Writer, name string, mode fs.FileMode, body []byte) error {
	if name != "+MANIFEST" {
		name = path.Clean(name)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     int64(mode.Perm()),
		Size:     int64(len(body)),
		Uname:    "root",
		Gname:    "wheel",
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}
