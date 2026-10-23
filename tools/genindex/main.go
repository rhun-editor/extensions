// genindex writes the rhun extension index: index.ini and its sha256sums.txt, from one INI file
// per extension (OWNER/NAME.ini, the file a pull request adds or changes). For each it fetches the
// pinned release's sha256sums.txt, checks every digest the file pins against it, downloads one
// archive (verified), and takes the title, description, color, version, permissions and pack list
// from the archive's extension.ini, which must name OWNER/NAME and the tag's version.
//
//	go run ./tools/genindex -out dist            # write dist/index.ini and dist/sha256sums.txt
//	go run ./tools/genindex -check               # check every entry, write nothing
//	-base URL                                    # releases somewhere else (tests); default GitHub
//
// The index format is the one rhun reads (docs/guide.md, "The rhun extensions index"): INI with
// whole-line comments, values on one line of at most 1000 bytes, at most 2000 entries.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	targets   = []string{"linux-x86_64", "macos-arm64", "windows-x64"}
	partRe    = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
	tagRe     = regexp.MustCompile(`^[0-9A-Za-z._-]{1,64}$`)
	digestRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	categories = map[string]bool{"languages": true, "formatters": true, "themes": true, "testing": true,
		"debugging": true, "tools": true, "packs": true, "other": true}
)

// ini is a section-less view of an INI file: key -> value (later keys win), and [section] keys as
// "section.key".
type ini map[string]string

func parseINI(data []byte) ini {
	m := ini{}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if section != "" {
			k = section + "." + k
		}
		m[k] = v
	}
	return m
}

func get(url string, max int64) ([]byte, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: more than %d bytes", url, max)
	}
	return data, nil
}

func sums(data []byte) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			m[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return m
}

// manifest reads extension.ini from a release archive (plain files and folders only).
func manifest(archive []byte) (ini, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("the archive holds a link or a device: %s", h.Name)
		}
		total += h.Size
		if total > 64<<20 {
			return nil, errors.New("the archive unpacks to more than 64 MB")
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "extension.ini" {
			data, err := io.ReadAll(io.LimitReader(tr, 65536))
			if err != nil {
				return nil, err
			}
			return parseINI(data), nil
		}
	}
	return nil, errors.New("the archive has no extension.ini")
}

// needs turns a manifest's [needs] into the index's KEY=VALUE words (lists comma separated).
func needs(m ini) string {
	var w []string
	for _, k := range []string{"documents", "project", "home"} {
		if v := m["needs."+k]; v == "read" || v == "write" {
			w = append(w, k+"="+v)
		}
	}
	for _, k := range []string{"network", "terminal", "audio"} {
		if m["needs."+k] == "yes" {
			w = append(w, k+"=yes")
		}
	}
	for _, k := range []string{"run", "tests"} {
		if v := strings.Fields(m["needs."+k]); len(v) > 0 {
			w = append(w, k+"="+strings.Join(v, ","))
		}
	}
	return strings.Join(w, " ")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

type entry struct {
	full  string
	lines []string
}

func build(base, path string) (entry, error) {
	rel, _ := filepath.Rel(".", path)
	full := strings.TrimSuffix(filepath.ToSlash(rel), ".ini")
	owner, name, ok := strings.Cut(full, "/")
	if !ok || !partRe.MatchString(owner) || !partRe.MatchString(name) {
		return entry{}, fmt.Errorf("%s: the file must be OWNER/NAME.ini", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return entry{}, err
	}
	pin := parseINI(data)
	tag := pin["tag"]
	if !tagRe.MatchString(tag) {
		return entry{}, fmt.Errorf("%s: tag %q is not a release tag", full, tag)
	}
	if c := pin["category"]; c != "" && !categories[c] {
		return entry{}, fmt.Errorf("%s: unknown category %q", full, c)
	}
	url := strings.TrimRight(base, "/") + "/" + full + "/releases/download/" + tag
	sumData, err := get(url+"/sha256sums.txt", 1<<20)
	if err != nil {
		return entry{}, fmt.Errorf("%s: %v", full, err)
	}
	released := sums(sumData)
	pinned := map[string]string{}
	for _, t := range targets {
		d := pin["sha256."+t]
		if d == "" {
			continue
		}
		if !digestRe.MatchString(d) {
			return entry{}, fmt.Errorf("%s: sha256.%s is not a digest", full, t)
		}
		if released[name+"-"+t+".tar.gz"] != d {
			return entry{}, fmt.Errorf("%s: sha256.%s is not the release's", full, t)
		}
		pinned[t] = d
	}
	if len(pinned) == 0 {
		return entry{}, fmt.Errorf("%s: no sha256.TARGET pinned", full)
	}
	// one archive, verified, for its manifest
	var t string
	for _, x := range targets {
		if pinned[x] != "" {
			t = x
			break
		}
	}
	archive, err := get(url+"/"+name+"-"+t+".tar.gz", 64<<20)
	if err != nil {
		return entry{}, fmt.Errorf("%s: %v", full, err)
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != pinned[t] {
		return entry{}, fmt.Errorf("%s: the %s archive does not match its digest", full, t)
	}
	m, err := manifest(archive)
	if err != nil {
		return entry{}, fmt.Errorf("%s: %v", full, err)
	}
	if !strings.EqualFold(m["owner"], owner) || !strings.EqualFold(m["name"], name) {
		return entry{}, fmt.Errorf("%s: its extension.ini names %s/%s", full, m["owner"], m["name"])
	}
	version := m["version"]
	if strings.TrimPrefix(tag, "v") != version {
		return entry{}, fmt.Errorf("%s: the tag %s is not its version %s", full, tag, version)
	}
	if m["title"] == "" {
		return entry{}, fmt.Errorf("%s: its extension.ini has no title", full)
	}
	e := entry{full: m["owner"] + "/" + m["name"]}
	add := func(k, v string) {
		if v != "" {
			e.lines = append(e.lines, k+" = "+oneLine(v))
		}
	}
	add("title", m["title"])
	add("description", m["description"])
	add("version", version)
	add("tag", tag)
	add("commit", pin["commit"])
	for _, x := range targets {
		add("sha256."+x, pinned[x])
	}
	add("color", m["color"])
	add("category", pin["category"])
	add("keywords", pin["keywords"])
	add("needs", needs(m))
	add("rhun", pin["rhun"])
	add("pack", m["pack"])
	if pin["featured"] == "yes" {
		add("featured", "yes")
	}
	add("maintainer", pin["maintainer"])
	return e, nil
}

func main() {
	base := flag.String("base", "https://github.com", "where the releases are")
	out := flag.String("out", "", "the folder index.ini and sha256sums.txt go to")
	check := flag.Bool("check", false, "check every entry; write nothing")
	flag.Parse()
	files, _ := filepath.Glob("*/*.ini")
	sort.Strings(files)
	var entries []entry
	failed := false
	for _, f := range files {
		if strings.HasPrefix(f, ".") || strings.HasPrefix(f, "tools/") || strings.HasPrefix(f, "pending/") {
			continue
		}
		e, err := build(*base, f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			failed = true
			continue
		}
		fmt.Fprintln(os.Stderr, "ok", e.full)
		entries = append(entries, e)
	}
	if len(entries) > 2000 {
		fmt.Fprintln(os.Stderr, "more than 2000 entries")
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	if *check || *out == "" {
		return
	}
	var b strings.Builder
	b.WriteString("# rhun extension index, generated by github.com/rhun-editor/extensions; do not edit\n")
	b.WriteString("format = 1\n")
	b.WriteString("generated = " + time.Now().UTC().Format("2006-01-02T15:04:05Z") + "\n")
	for _, e := range entries {
		b.WriteString("\n[" + e.full + "]\n")
		for _, l := range e.lines {
			b.WriteString(l + "\n")
		}
	}
	if b.Len() > 1<<20 {
		fmt.Fprintln(os.Stderr, "the index is larger than 1 MB")
		os.Exit(1)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data := []byte(b.String())
	sum := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(*out, "index.ini"), data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	line := hex.EncodeToString(sum[:]) + "  index.ini\n"
	if err := os.WriteFile(filepath.Join(*out, "sha256sums.txt"), []byte(line), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
