// HugoDesk — a portable local GUI editor for any Hugo blog.
// Run the binary inside any Hugo site root and open http://127.0.0.1:8765
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- site state

var (
	root string // hugo site root (cwd where the binary is launched)
	tmpl *template.Template
)

type Post struct {
	Path     string // path relative to root, slash separated
	Section  string
	Title    string
	Date     string
	Draft    bool
	Summary  string
	Tags     []string
	Cats     []string
	Body     string
	Extras   [][2]string // other simple scalar keys preserved verbatim
	Complex  bool        // front matter we can't structure-edit
	ComplexF string      // raw front matter text (complex mode)
	Warning  string
	Exists   bool
}

var managedKeys = map[string]bool{
	"title": true, "date": true, "draft": true,
	"summary": true, "tags": true, "categories": true,
}

// ------------------------------------------------------------- front matter

func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	b, _ := json.Marshal(s)
	return string(b)
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func isScalar(v string) (string, bool) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", false
	}
	if (strings.HasPrefix(t, `"`) && strings.HasSuffix(t, `"`) && len(t) >= 2) ||
		(strings.HasPrefix(t, `'`) && strings.HasSuffix(t, `'`) && len(t) >= 2) {
		return t[1 : len(t)-1], true
	}
	if t == "true" || t == "false" {
		return t, true
	}
	if _, err := strconv.Atoi(t); err == nil {
		return t, true
	}
	if _, err := strconv.ParseFloat(t, 64); err == nil {
		return t, true
	}
	// bare words / dates are fine as scalars
	if !strings.ContainsAny(t, "{}[]#:|>&*!%@`,") {
		return t, true
	}
	return "", false
}

func parseScalarList(v string) []string {
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	parts := splitCommas(v)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.Trim(p, `"'`)
		out = append(out, p)
	}
	return out
}

func splitCommas(s string) []string {
	var parts []string
	depth, start := 0, 0
	inQ := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQ != 0 {
			if c == inQ {
				inQ = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQ = c
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// readFM parses simple front matter (yaml --- or toml +++) into
// managed fields + preserved extras. Complex FM is kept raw.
func readFM(src string) (fm map[string]string, order []string, body string,
	complexRaw string, warning string) {

	fm = map[string]string{}
	lines := strings.Split(src, "\n")
	if len(lines) == 0 {
		return fm, order, src, "", ""
	}
	first := strings.TrimSpace(lines[0])
	var mode, closer string
	switch first {
	case "---":
		mode, closer = "yaml", "---"
	case "+++":
		mode, closer = "toml", "+++"
	default:
		return fm, order, src, "", "no front matter"
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == closer {
			end = i
			break
		}
	}
	if end == -1 {
		return fm, order, src, "", "unclosed front matter"
	}
	body = strings.TrimPrefix(strings.Join(lines[end+1:], "\n"), "\n")

	sep := ":"
	if mode == "toml" {
		sep = "="
	}
	var pending []string // accumulated lines of a multi-line value
	flushMultiline := func() {
		if len(pending) > 0 {
			warning = "some front matter values span multiple lines and are only preserved as raw text"
			complexRaw += strings.Join(pending, "\n") + "\n"
			pending = nil
		}
	}
	for _, ln := range lines[1:end] {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if pending != nil {
			pending = append(pending, ln)
			joined := strings.Join(pending, " ")
			if strings.Contains(joined, "]") {
				// will be handled on next pass via re-parse below
				flushMultiline()
			}
			continue
		}
		k, val, ok := splitKV(trimmed, sep)
		if !ok {
			flushMultiline()
			warning = "some front matter is advanced (nested/tables) and is only preserved as raw text"
			complexRaw += ln + "\n"
			continue
		}
		if mode == "toml" && strings.HasPrefix(val, "[") && !strings.Contains(val, "]") {
			warning = "multi-line values are preserved as raw text"
			complexRaw += k + " = " + val
			pending = []string{val}
			continue
		}
		if strings.HasPrefix(val, "[") {
			fm[k] = "[" + strings.Join(parseScalarList(val), ", ") + "]"
		} else if sc, ok := isScalar(val); ok {
			fm[k] = sc
		} else if mode == "yaml" {
			warning = "some front matter values are complex and are only preserved as raw text"
			complexRaw += k + ": " + val + "\n"
		} else {
			warning = "some front matter values are complex and are only preserved as raw text"
			complexRaw += k + " = " + val + "\n"
		}
	}
	flushMultiline()
	// keep insertion order for extras
	seen := map[string]bool{}
	_ = seen
	return fm, order, body, complexRaw, warning
}

func splitKV(s, sep string) (key, val string, ok bool) {
	i := strings.Index(s, sep)
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	val = strings.TrimSpace(s[i+1:])
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, val, true
}

func buildFM(p *Post) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlQuote(p.Title))
	if p.Date != "" {
		if dateRe.MatchString(p.Date) {
			fmt.Fprintf(&b, "date: %s\n", p.Date)
		} else {
			fmt.Fprintf(&b, "date: %s\n", yamlQuote(p.Date))
		}
	}
	for _, c := range p.Cats {
		_ = c
	}
	if len(p.Cats) > 0 {
		quoted := make([]string, len(p.Cats))
		for i, c := range p.Cats {
			quoted[i] = yamlQuote(c)
		}
		fmt.Fprintf(&b, "categories: [%s]\n", strings.Join(quoted, ", "))
	}
	if len(p.Tags) > 0 {
		quoted := make([]string, len(p.Tags))
		for i, t := range p.Tags {
			quoted[i] = yamlQuote(t)
		}
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(quoted, ", "))
	}
	if p.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", yamlQuote(p.Summary))
	}
	fmt.Fprintf(&b, "draft: %t\n", p.Draft)
	for _, kv := range p.Extras {
		fmt.Fprintf(&b, "%s: %s\n", kv[0], kv[1])
	}
	b.WriteString("---\n")
	b.WriteString(p.Body)
	if !strings.HasSuffix(p.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func readPost(rel string) (*Post, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	src := string(data)
	p := &Post{Path: rel, Section: sectionOf(rel), Exists: true}
	extrasFM, _, body, complexRaw, warn := readFM(src)
	p.Body = body
	p.Complex = complexRaw != ""
	p.ComplexF = complexRaw
	p.Warning = warn
	// managed keys
	p.Title = extrasFM["title"]
	p.Summary = extrasFM["summary"]
	if d, ok := extrasFM["date"]; ok {
		p.Date = d
	}
	if extrasFM["draft"] == "true" {
		p.Draft = true
	}
	if v, ok := extrasFM["tags"]; ok {
		p.Tags = parseScalarList(v)
	}
	if v, ok := extrasFM["categories"]; ok {
		p.Cats = parseScalarList(v)
	}
	// extras (other simple scalars, keep raw yaml value form)
	rawLines := fmRawLines(src)
	for _, ln := range rawLines {
		k, v, ok := splitKV(ln, keySep(ln))
		if !ok || managedKeys[strings.ToLower(k)] {
			continue
		}
		if sc, ok := isScalar(v); ok {
			p.Extras = append(p.Extras, [2]string{k, sc})
		}
	}
	return p, nil
}

func keySep(ln string) string {
	// shared between yaml/toml simple line handling
	if strings.Contains(ln, "=") && !strings.Contains(strings.SplitN(ln, "=", 2)[0], ":") {
		return "="
	}
	if strings.Contains(ln, ":") {
		return ":"
	}
	return "="
}

func fmRawLines(src string) []string {
	lines := strings.Split(src, "\n")
	if len(lines) == 0 {
		return nil
	}
	first := strings.TrimSpace(lines[0])
	closer := ""
	if first == "---" {
		closer = "---"
	} else if first == "+++" {
		closer = "+++"
	} else {
		return nil
	}
	var out []string
	for _, ln := range lines[1:] {
		if strings.TrimSpace(ln) == closer {
			break
		}
		out = append(out, strings.TrimSpace(ln))
	}
	return out
}

func writePost(rel string, content string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func deletePost(rel string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	return os.Remove(abs)
}

func sectionOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return "."
	}
	return parts[1]
}

func sections() []string {
	var out []string
	entries, err := os.ReadDir(filepath.Join(root, "content"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

type Row struct {
	Path    string
	Section string
	Title   string
	Date    string
	Draft   bool
	Preview string
}

func allPosts() []Row {
	var rows []Row
	_ = filepath.WalkDir(filepath.Join(root, "content"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") && !strings.HasSuffix(d.Name(), ".markdown") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		p, err := readPost(rel)
		if err != nil {
			return nil
		}
		rows = append(rows, Row{Path: rel, Section: p.Section,
			Title: p.Title, Date: p.Date, Draft: p.Draft,
			Preview: previewText(p)})
		return nil
	})
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Date != rows[j].Date {
			return rows[i].Date > rows[j].Date
		}
		return rows[i].Path < rows[j].Path
	})
	return rows
}

func previewText(p *Post) string {
	t := strings.TrimSpace(strings.TrimLeft(p.Body, "#>-*`0123456789. "))
	return firstWords(t, 12)
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = w[:n]
	}
	return strings.Join(w, " ")
}

// ------------------------------------------------------------- slug helper

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "post"
	}
	return s
}

func freeSlug(section, base string) string {
	base = slugify(base)
	cand := base
	for i := 2; ; i++ {
		rel := "content/" + path2(section, cand) + ".md"
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); os.IsNotExist(err) {
			return rel
		}
		cand = fmt.Sprintf("%s-%d", base, i)
	}
}

func path2(section, name string) string {
	if section == "" || section == "." {
		return name
	}
	return section + "/" + name
}

func safePath(rel string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	clean := filepath.Clean(abs)
	rp, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(clean, rp+string(os.PathSeparator)) && clean != rp {
		return errors.New("path outside site root")
	}
	if !strings.HasPrefix(clean+string(os.PathSeparator), filepath.Join(rp, "content")+string(os.PathSeparator)) {
		return errors.New("only files under content/ can be managed")
	}
	if _, err := os.Stat(clean); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ------------------------------------------------------------- preview server

var (
	previewCmd   *exec.Cmd
	previewURL   string
	previewMode  string // "published" or "drafts"
	previewMutex = make(chan bool, 1)
	hugoMissing  bool
)

func stopPreview() {
	if previewCmd != nil && previewCmd.Process != nil {
		_ = previewCmd.Process.Kill()
	}
	previewCmd, previewURL, previewMode = nil, "", ""
}

func startPreview(mode string) (string, error) {
	previewMutex <- true
	defer func() { <-previewMutex }()
	hugo, err := exec.LookPath("hugo")
	if err != nil {
		hugoMissing = true
		return "", errors.New("hugo not found in PATH — install Hugo to preview the site")
	}
	if previewCmd != nil && previewCmd.Process != nil && previewURL != "" {
		if previewMode == mode {
			return previewURL, nil
		}
		stopPreview()
		time.Sleep(300 * time.Millisecond)
	}
	args := []string{"server", "--bind", "127.0.0.1"}
	if mode == "drafts" {
		args = append(args, "-D")
	}
	for port := 4321; port < 4361; port++ {
		u := fmt.Sprintf("http://127.0.0.1:%d/", port)
		cmd := exec.Command(hugo, append(args, "--port", strconv.Itoa(port), "--baseURL", u)...)
		cmd.Dir = root
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			continue
		}
		ok := false
		for i := 0; i < 20; i++ {
			time.Sleep(250 * time.Millisecond)
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				break
			}
			client := &http.Client{Timeout: 400 * time.Millisecond}
			resp, err := client.Get(u)
			if err == nil {
				resp.Body.Close()
				ok = true
				break
			}
		}
		if ok && cmd.Process != nil {
			previewCmd = cmd
			previewURL = u
			previewMode = mode
			return u, nil
		}
		_ = cmd.Process.Kill()
	}
	return "", errors.New("could not start hugo server")
}

// ------------------------------------------------------------- http handlers

type Group struct {
	Name string
	Rows []Row
}

func errPage(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	renderInner(w, errT, "error", map[string]any{"Message": msg}, false)
}

func handleDash(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		errPage(w, 404, "Page not found")
		return
	}
	rows := allPosts()
	bySec := map[string][]Row{}
	var order []string
	known := map[string]bool{}
	for _, s := range sections() {
		order = append(order, s)
		known[s] = true
	}
	for _, row := range rows {
		if !known[row.Section] {
			known[row.Section] = true
			order = append(order, row.Section)
		}
		bySec[row.Section] = append(bySec[row.Section], row)
	}
	var groups []Group
	for _, s := range order {
		groups = append(groups, Group{Name: s, Rows: bySec[s]})
	}
	data := map[string]any{
		"Site":     filepath.Base(root),
		"Groups":   groups,
		"Sections": sections(),
		"NoHugo":   hugoMissing,
	}
	renderInner(w, dashT, "dash", data, previewURL != "")
}

func handleEdit(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("f")
	if rel == "" {
		errPage(w, 400, "missing file")
		return
	}
	if err := safePath(rel); err != nil {
		errPage(w, 403, err.Error())
		return
	}
	p, err := readPost(rel)
	if err != nil {
		errPage(w, 404, "cannot read post: "+err.Error())
		return
	}
	_, saved := r.URL.Query()["saved"]
	renderEditor(w, p, false, saved)
}

func handleNew(w http.ResponseWriter, r *http.Request) {
	sec := r.URL.Query().Get("section")
	if sec == "" {
		sec = firstSection()
	}
	p := &Post{Section: sec, Date: time.Now().Format("2006-01-02")}
	renderEditor(w, p, true, false)
}

func firstSection() string {
	if s := sections(); len(s) > 0 {
		return s[0]
	}
	return ""
}

func renderEditor(w http.ResponseWriter, p *Post, isNew, saved bool) {
	extras, _ := json.Marshal(p.Extras)
	data := map[string]any{
		"P":          p,
		"IsNew":      isNew,
		"Site":       filepath.Base(root),
		"Sections":   sections(),
		"TagsStr":    strings.Join(p.Tags, ", "),
		"CatsStr":    strings.Join(p.Cats, ", "),
		"ExtrasJSON": string(extras),
		"Saved":      saved,
	}
	renderInner(w, editorT, "editor", data, false)
}

func handleSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errPage(w, 400, "bad form")
		return
	}
	f := r.PostForm
	rel := f.Get("path")
	isNew := f.Get("isnew") == "1"

	tags := parseList(f.Get("tags"))
	cats := parseList(f.Get("cats"))
	extras := parseExtras(f.Get("extras"))

	title := strings.TrimSpace(f.Get("title"))
	if title == "" {
		errPage(w, 400, "title is required")
		return
	}

	var target string
	var err error
	if isNew {
		sec := f.Get("section")
		slug := strings.TrimSpace(f.Get("slug"))
		if slug != "" {
			target = freeSlug(sec, slug)
		} else {
			target = freeSlug(sec, title)
		}
	} else {
		target = rel
	}

	p := &Post{
		Path:    target,
		Title:   title,
		Date:    f.Get("date"),
		Draft:   f.Get("draft") == "on",
		Summary: f.Get("summary"),
		Tags:    tags,
		Cats:    cats,
		Body:    f.Get("body"),
		Extras:  extras,
	}

	if err = safePath(target); err != nil {
		errPage(w, 403, err.Error())
		return
	}
	content := buildFM(p)
	if err = writePost(target, content); err != nil {
		errPage(w, 500, "save failed: "+err.Error())
		return
	}
	http.Redirect(w, r, "/edit?f="+url.QueryEscape(target)+"&saved=1", http.StatusSeeOther)
}

func handleRawSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rel := r.PostForm.Get("path")
	body := r.PostForm.Get("body")
	raw := r.PostForm.Get("fmraw")
	if err := safePath(rel); err != nil {
		errPage(w, 403, err.Error())
		return
	}
	content := raw + "\n" + body
	if err := writePost(rel, content); err != nil {
		errPage(w, 500, "save failed: "+err.Error())
		return
	}
	http.Redirect(w, r, "/edit?f="+url.QueryEscape(rel)+"&saved=1", http.StatusSeeOther)
}

func parseList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseExtras(s string) [][2]string {
	var in []struct {
		K string `json:"k"`
		V string `json:"v"`
	}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &in)
	}
	var out [][2]string
	for _, e := range in {
		out = append(out, [2]string{e.K, e.V})
	}
	return out
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rel := r.PostForm.Get("f")
	if err := safePath(rel); err != nil {
		errPage(w, 403, err.Error())
		return
	}
	p, err := readPost(rel)
	if err != nil {
		errPage(w, 404, err.Error())
		return
	}
	if p.Complex {
		errPage(w, 400, "advanced front matter — please edit this file manually")
		return
	}
	p.Draft = !p.Draft
	if err := writePost(rel, buildFM(p)); err != nil {
		errPage(w, 500, err.Error())
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rel := r.PostForm.Get("f")
	if err := safePath(rel); err != nil {
		errPage(w, 403, err.Error())
		return
	}
	if err := deletePost(rel); err != nil {
		errPage(w, 500, err.Error())
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	mode := "published"
	if r.URL.Query().Get("drafts") == "1" {
		mode = "drafts"
	}
	u, err := startPreview(mode)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"url": u})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	stopPreview()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ------------------------------------------------------------- templates

const css = `
:root{--bg:#f3f0ea;--panel:#fff;--ink:#17130f;--soft:#453f38;--muted:#77716a;--line:#d8d2c7}
*{box-sizing:border-box;margin:0;padding:0}
body{background:var(--bg);color:var(--ink);font:16px/1.6 Georgia,"Times New Roman",serif;-webkit-font-smoothing:antialiased}
.ui{font-family:system-ui,-apple-system,"Segoe UI",sans-serif}
.top{display:flex;align-items:center;gap:1.2rem;padding:.9rem 1.5rem;border-bottom:2px solid var(--ink);max-width:80rem;margin:0 auto}
.wordmark{font-size:1.35rem;font-weight:800;letter-spacing:-.01em;text-decoration:none;color:var(--ink)}
.top a.action{font-family:system-ui,sans-serif;font-size:.85rem;font-weight:600;padding:.45rem 1rem;background:var(--ink);color:#fff;border:none;border-radius:6px;text-decoration:none;cursor:pointer}
.top a.action.ghost{background:none;color:var(--ink);border:1px solid var(--line)}
.wrap{max-width:80rem;margin:0 auto;padding:2rem 1.5rem}
h2.sec{font-size:.72rem;letter-spacing:.18em;text-transform:uppercase;font-family:system-ui,sans-serif;color:var(--muted);margin:1.6rem 0 .6rem;border-bottom:1px solid var(--line);padding-bottom:.4rem}
table.list{width:100%;border-collapse:collapse;font-family:system-ui,sans-serif}
table.list td{border-bottom:1px solid var(--line);padding:.65rem .5rem;font-size:.92rem}
table.list td.t{font-family:Georgia,serif;font-size:1.05rem;font-weight:700}
a.plain{color:var(--ink);text-decoration:none}
a.plain:hover{color:var(--muted)}
.badge{display:inline-block;font-size:.68rem;font-weight:700;letter-spacing:.1em;padding:.15rem .5rem;border-radius:3px;background:#e9e4db;color:#77716a}
.badge.draft{background:#3d3931;color:#fff}
.meta{color:var(--muted);font-size:.8rem;white-space:nowrap}
.forms{display:flex!important;gap:.4rem}
.forms button,.forms a{font-size:.78rem;font-family:system-ui,sans-serif;border:1px solid var(--line);background:none;border-radius:4px;padding:.25rem .7rem;cursor:pointer;color:var(--ink);text-decoration:none}
.forms button:hover,.forms a:hover{border-color:var(--ink)}
.ed {display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:1.5rem}
@media(max-width:1000px){.ed{grid-template-columns:1fr}}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:1.3rem}
label.f{display:block;font-family:system-ui,sans-serif;font-size:.72rem;font-weight:700;letter-spacing:.12em;text-transform:uppercase;color:var(--muted);margin:.9rem 0 .3rem}
input[type=text],input[type=date],select,textarea{width:100%;font-family:system-ui,sans-serif;font-size:.95rem;padding:.5rem .7rem;border:1px solid var(--line);border-radius:6px;background:#fff;color:#17130f}
textarea.body{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.9rem;min-height:26rem;line-height:1.55;resize:vertical}
.row3{display:grid;grid-template-columns:1fr 1fr;gap:1rem}
.check{display:flex;align-items:center;gap:.5rem;margin-top:1.2rem;font-family:system-ui,sans-serif;font-size:.9rem}
.check input{width:auto}
.btnrow{display:flex;gap:.7rem;align-items:center;margin-top:1.4rem;flex-wrap:wrap}
button.save{font-weight:600;padding:.7rem 1.4rem;background:var(--ink);color:#fff;border:none;border-radius:6px;font-size:.95rem;cursor:pointer}
button.save:hover{opacity:.85}
a.minor{font-family:system-ui,sans-serif;font-size:.88rem;color:var(--muted)}
.pv-h{font-family:system-ui,sans-serif;font-size:.72rem;font-weight:700;letter-spacing:.15em;text-transform:uppercase;color:var(--muted);margin-bottom:.6rem}
.pv{border:1px solid var(--line);border-radius:8px;background:#fff;padding:1.2rem 1.4rem;min-height:26rem;overflow-y:auto;max-height:44rem}
.pv h1,.pv h2,.pv h3{font-family:Georgia,serif;margin:.9em 0 .4em;line-height:1.25}
.pv h1{font-size:1.5rem}.pv h2{font-size:1.28rem}.pv h3{font-size:1.1rem}
.pv p{margin:.7em 0;color:#453f38}
.pv code{background:#f0ede6;border-radius:4px;padding:.1em .35em;font-size:.9em}
.pv pre{background:#f0ede6;padding:.9rem;border-radius:6px;overflow-x:auto;margin:.8em 0}
.pv blockquote{border-left:3px solid #17130f;background:#f6f3ed;padding:.7rem 1rem;margin:.8em 0;font-style:italic}
.pv ul,.pv ol{margin:.6em 0 .6em 1.4em;color:#453f38}
.pv img{max-width:100%}
.pv table{width:100%;border-collapse:collapse;margin:.9em 0;font-family:system-ui,sans-serif;font-size:.86rem}
.pv th{background:#f0ede6;border-bottom:2px solid #17130f;padding:.45rem .6rem;text-align:left;font-size:.7rem;font-weight:700;letter-spacing:.09em;text-transform:uppercase}
.pv td{border-bottom:1px solid #d8d2c7;padding:.45rem .6rem;color:#453f38}
.notice{background:#e9e4db;border-left:3px solid #17130f;padding:.6rem .9rem;font-family:system-ui,sans-serif;font-size:.85rem;margin-bottom:1rem}
.saved{background:#17130f;color:#f3f0ea}
.extras{font-family:system-ui,sans-serif;font-size:.8rem;color:var(--muted);margin-top:1rem}
.extras code{background:#f0ede6;padding:.05rem .3rem;border-radius:3px}
`

const layoutBase = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>{{.CSS}}</style>
</head>
<body>
<div class="top ui">
  <a class="wordmark" href="/">Hugo<span style="color:#77716a">Desk</span></a>
  <span style="color:#77716a;font-family:system-ui,sans-serif;font-size:.85rem">{{.Site}}</span>
  <span style="flex:1"></span>
  <a class="action ghost" id="preview-btn" href="#" onclick="startPreview(false);return false">Preview site</a>
  <a class="action ghost" id="preview-drafts-btn" href="#" onclick="startPreview(true);return false">Preview drafts</a>
  {{if .Stopable}}<a class="action ghost" href="/stop">Stop preview</a>{{end}}
  <a class="action" href="/new">&#43; New post</a>
</div>
{{.Inner}}
<script>
function startPreview(drafts){
  var b=drafts?document.getElementById('preview-drafts-btn'):document.getElementById('preview-btn');
  b.textContent='Starting\u2026';
  fetch('/preview'+(drafts?'?drafts=1':'')).then(r=>r.json()).then(function(d){
    if(d.url){window.open(d.url,'_blank');b.textContent=drafts?'Preview drafts':'Preview site';}
    else{b.textContent=d.error;setTimeout(function(){b.textContent=drafts?'Preview drafts':'Preview site';},3000);}
  });
}
</script>
</body>
</html>`

const dashInner = `
<div class="wrap">
  {{if .NoHugo}}<div class="notice ui">Hugo binary not found in PATH — editing works, but site preview is unavailable.</div>{{end}}
  {{range .Groups}}
    <h2 class="sec">content / {{.Name}}</h2>
    {{if .Rows}}
    <table class="list">
      {{range .Rows}}
      <tr>
        <td class="t"><a class="plain" href="/edit?f={{.Path}}">{{.Title}}</a></td>
        <td>{{if .Draft}}<span class="badge draft ui">DRAFT</span>{{else}}<span class="badge ui">PUBLISHED</span>{{end}}</td>
        <td class="meta ui">{{.Date}}</td>
        <td class="ui" style="text-align:right"><span class="forms">
          <a href="/edit?f={{.Path}}">Edit</a>
          <form method="post" action="/toggle" style="display:inline"><input type="hidden" name="f" value="{{.Path}}"><button type="submit">{{if .Draft}}Publish{{else}}Unpublish{{end}}</button></form>
          <form method="post" action="/delete" style="display:inline" onsubmit="return confirm('Delete {{.Path}}?')"><input type="hidden" name="f" value="{{.Path}}"><button type="submit">Delete</button></form>
        </span></td>
      </tr>
      {{end}}
    </table>
    {{end}}
  {{end}}
</div>`

const editorInner = `
<div class="wrap">
  {{if .Saved}}<div class="notice saved ui">Saved &#10003;</div>{{end}}
  {{if .P.Warning}}<div class="notice ui">{{.P.Warning}}</div>{{end}}
  <form method="post" action="/save" class="ed">
    <div class="panel">
      <input type="hidden" name="path" value="{{.P.Path}}">
      <input type="hidden" name="isnew" value="{{if .IsNew}}1{{else}}0{{end}}">
      <input type="hidden" name="extras" id="extras" value="{{.ExtrasJSON}}">

      {{if .IsNew}}
      <label class="f">Section</label>
      <select name="section">{{range .Sections}}<option value="{{.}}" {{if eq . $.P.Section}}selected{{end}}>content/{{.}}</option>{{end}}<option value="" {{if eq "" $.P.Section}}selected{{end}}>content (root)</option></select>
      <label class="f">Filename slug (optional — derived from title by default)</label>
      <input type="text" name="slug" placeholder="auto">
      {{else}}
      <div class="ui" style="font-size:.8rem;color:#77716a">content/{{.P.Path}}</div>
      {{end}}

      <label class="f">Title</label>
      <input type="text" name="title" id="title" value="{{.P.Title}}" required>

      <div class="row3">
        <div><label class="f">Date</label><input type="date" name="date" value="{{.P.Date}}"></div>
        <div><label class="f">Categories (comma separated)</label><input type="text" name="cats" value="{{.CatsStr}}"></div>
      </div>

      <label class="f">Tags (comma separated)</label>
      <input type="text" name="tags" value="{{.TagsStr}}">

      <label class="f">Summary</label>
      <textarea name="summary" rows="2">{{.P.Summary}}</textarea>

      <label class="f">Body (Markdown)</label>
      <textarea class="body" name="body" id="body" oninput="pv()">{{.P.Body}}</textarea>

      <div class="check"><input type="checkbox" name="draft" id="draft" {{if .P.Draft}}checked{{end}}><label for="draft">Draft (checked = not published)</label></div>

      <div class="btnrow">
        <button class="save" type="submit">Save</button>
        <a class="minor" href="/">Cancel</a>
        {{if .P.Extras}}<span class="extras">preserved: {{range .P.Extras}}<code>{{index . 0}}</code> {{end}}</span>{{end}}
      </div>
    </div>
  </form>

  <div>
    <p class="pv-h">Live preview</p>
    <div class="pv" id="pv"></div>
  </div>
</div>
<script>
function esc(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')}
function pv(){
  var src=document.getElementById('body').value;
  document.getElementById('pv').innerHTML=md(src);
}
function md(src){
  var out=[];var lines=esc(src).split('\n');var i=0;
  function inline(s){return s.replace(/\*\*(.+?)\*\*/g,'<strong>$1</strong>').replace(/\*(.+?)\*/g,'<em>$1</em>').replace(/\u0060([^\u0060]+)\u0060/g,'<code>$1</code>').replace(/\[([^\]]+)\]\(([^)]+)\)/g,'<a href="$2">$1</a>')}
  while(i<lines.length){
    var l=lines[i];
    if(l.startsWith('\u0060\u0060\u0060')){var buf=[];i++;while(i<lines.length&&!lines[i].startsWith('\u0060\u0060\u0060')){buf.push(lines[i]);i++}i++;out.push('<pre><code>'+buf.join('\n')+'</code></pre>');continue}
    var h=l.match(/^(#{1,3})\s+(.*)/);
    if(h){out.push('<h'+h[1].length+'>'+inline(h[2])+'</h'+h[1].length+'>');i++;continue}
    if(/^(-{3,}|\*{3,})\s*$/.test(l)){out.push('<hr>');i++;continue}
    if(l.startsWith('> ')){var q=[];while(i<lines.length&&lines[i].startsWith('> ')){q.push(lines[i].slice(2));i++}out.push('<blockquote>'+inline(q.join(' '))+'</blockquote>');continue}
    if(/^[-*]\s+/.test(l)||/^\d+\.\s+/.test(l)){
      var ol=/^\d+\.\s+/.test(l);var tag=ol?'<ol>':'<ul>';var items=[];
      while(i<lines.length&&(/^[-*]\s+/.test(lines[i])||/^\d+\.\s+/.test(lines[i]))){items.push('<li>'+inline(lines[i].replace(/^([-*]|\d+\.)\s+/,''))+'</li>');i++}
      out.push(tag+items.join('')+(ol?'</ol>':'</ul>'));continue}
    if(l.indexOf('|')>-1&&i+1<lines.length&&/^\s*\|?[\s:-]*-{2,}[-\s|:]*$/.test(lines[i+1])&&lines[i+1].indexOf('|')>-1){
      var ths=mdCells(l);var rows=[];
      i+=2;
      while(i<lines.length&&lines[i].indexOf('|')>-1&&lines[i].trim()!==''){rows.push(mdCells(lines[i]));i++}
      out.push('<table><thead><tr>'+ths.map(function(c){return '<th>'+inline(c)+'</th>'}).join('')+'</tr></thead><tbody>'+rows.map(function(r){return '<tr>'+r.map(function(c){return '<td>'+inline(c)+'</td>'}).join('')+'</tr>'}).join('')+'</tbody></table>');continue}
    if(l.trim()===''){i++;continue}
    var para=[l];
    while(i+1<lines.length&&lines[i+1].trim()!==''&&!/^(#{1,3}\s|>|\u0060\u0060\u0060|[-*]\s|\d+\.\s|\|)/.test(lines[i+1])){i++;para.push(lines[i])}
    out.push('<p>'+inline(para.join(' '))+'</p>');i++;
  }
  return out.join('\n')
}
function mdCells(row){
  row=row.trim();
  if(row.charAt(0)==='|'){row=row.slice(1)}
  if(row.charAt(row.length-1)==='|'){row=row.slice(0,-1)}
  return row.split('|').map(function(c){return c.trim()})
}
pv();
</script>`

const errInner = `
<div class="wrap">
  <div class="notice ui">{{.Message}}</div>
  <p class="ui" style="margin-top:1rem"><a class="minor" href="/">&larr; Back</a></p>
</div>`

// ------------------------------------------------------------- template glue

type Page struct {
	Title    string
	Site     string
	CSS      template.CSS
	Inner    template.HTML
	Stopable bool
}

var dashT, editorT, errT, layoutT *template.Template

func parseLayout() {
	dashT = template.Must(template.New("dash").Parse(dashInner))
	editorT = template.Must(template.New("editor").Parse(editorInner))
	errT = template.Must(template.New("error").Parse(errInner))
	layoutT = template.Must(template.New("layout").Parse(layoutBase))
}

// renderInner renders one of the inner page templates into a buffer,
// then wraps it in the shared layout.
func renderInner(w http.ResponseWriter, t *template.Template, name string, data any, stopable bool) {
	var sub strings.Builder
	if err := t.Execute(&sub, data); err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	page := Page{
		Title:    "HugoDesk — " + filepath.Base(root),
		Site:     filepath.Base(root),
		CSS:      template.CSS(css),
		Inner:    template.HTML(sub.String()),
		Stopable: stopable,
	}
	if err := layoutT.Execute(w, page); err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
	}
}

// ------------------------------------------------------------- main

func main() {
	port := flag.Int("port", 8765, "port for the editor UI")
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot determine working directory:", err)
		os.Exit(1)
	}
	root = cwd
	if _, err := os.Stat(filepath.Join(root, "content")); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "no content/ directory here — run hugodesk from inside a Hugo site root")
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(root, "hugo.toml")); err != nil {
		if _, err2 := os.Stat(filepath.Join(root, "config.toml")); err2 != nil {
			if _, err3 := os.Stat(filepath.Join(root, "hugo.yaml")); err3 != nil {
				if _, err4 := os.Stat(filepath.Join(root, "config.yaml")); err4 != nil {
					fmt.Fprintln(os.Stderr, "warning: no hugo config found (hugo.toml/config.toml/hugo.yaml/config.yaml) — continuing anyway")
				}
			}
		}
	}

	parseLayout()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDash)
	mux.HandleFunc("/new", handleNew)
	mux.HandleFunc("/edit", handleEdit)
	mux.HandleFunc("/save", handleSave)
	mux.HandleFunc("/toggle", handleToggle)
	mux.HandleFunc("/delete", handleDelete)
	mux.HandleFunc("/preview", handlePreview)
	mux.HandleFunc("/stop", handleStop)

	u := "http://" + addr
	fmt.Println("HugoDesk  " + u)
	fmt.Println("Site:     " + root)
	fmt.Println("Stop:     Ctrl+C")
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		time.Sleep(600 * time.Millisecond)
		openBrowser(u)
	}()

	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch {
	case isCmd("xdg-open"):
		cmd = exec.Command("xdg-open", u)
	case isCmd("open"):
		cmd = exec.Command("open", u)
	default:
		return
	}
	_ = cmd.Start()
}

func isCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
