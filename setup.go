package main

import (
	"archive/zip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// First-run setup. The release ships the app exe plus:
//   - the llama.cpp CUDA 12.4 zip and the cudart zip  -> extracted into bin/
//   - the 0.8B helper GGUF                           -> models/
//   - the 9B GGUF split into <2 GB .partNN files      -> joined into models/
// and a manifest with SHA-256 for every file. The manifest is also compiled
// into the exe, so the exe alone knows exactly what it needs.

//go:embed manifest.json
var embeddedManifest []byte

type ManifestPart struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ManifestItem struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"` // "model" or "zip"
	Target string         `json:"target"`
	Size   int64          `json:"size"`
	SHA256 string         `json:"sha256"`
	Parts  []ManifestPart `json:"parts"`
}

type Manifest struct {
	Tag   string         `json:"tag"`
	Repo  string         `json:"repo"`
	Items []ManifestItem `json:"items"`
}

func loadManifest() (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(stripBOM(embeddedManifest), &m); err != nil {
		return nil, err
	}
	if m.Repo == "" {
		m.Repo = ReleaseRepo
	}
	if m.Tag == "" {
		m.Tag = ReleaseTag
	}
	return &m, nil
}

// installed reports whether the engine and both models are in place.
func installed() bool {
	for _, p := range []string{llamaExe(), modelPath(MainModelFile), modelPath(HelperModelFile)} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

type setupStatus struct {
	mu       sync.Mutex
	Running  bool      `json:"running"`
	Done     bool      `json:"done"`
	Error    string    `json:"error"`
	Step     string    `json:"step"`
	Bytes    int64     `json:"bytes"`
	Total    int64     `json:"total"`
	Log      []string  `json:"log"`
	Started  time.Time `json:"started"`
	Relaunch string    `json:"relaunch,omitempty"`
}

var setup = &setupStatus{}

func (s *setupStatus) logLine(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	logf("setup", "%s", line)
	s.mu.Lock()
	s.Log = append(s.Log, line)
	if len(s.Log) > 200 {
		s.Log = s.Log[len(s.Log)-200:]
	}
	s.mu.Unlock()
}

func (s *setupStatus) reset() {
	s.mu.Lock()
	s.Running, s.Done, s.Error, s.Step, s.Log, s.Relaunch = false, false, "", "", nil, ""
	s.mu.Unlock()
}

func (s *setupStatus) setStep(step string, total int64) {
	s.mu.Lock()
	s.Step, s.Bytes, s.Total = step, 0, total
	s.mu.Unlock()
}

func (s *setupStatus) add(n int64) {
	s.mu.Lock()
	s.Bytes += n
	s.mu.Unlock()
}

func (s *setupStatus) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"running": s.Running, "done": s.Done, "error": s.Error, "step": s.Step,
		"bytes": s.Bytes, "total": s.Total, "log": append([]string(nil), s.Log...),
		"relaunch": s.Relaunch,
	}
}

// searchDirs are where downloaded release files are looked for.
func searchDirs() []string {
	dirs := []string{appDir, filepath.Join(appDir, "download"), exeDir}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Downloads"))
	}
	seen := map[string]bool{}
	out := []string{}
	for _, d := range dirs {
		if a, err := filepath.Abs(d); err == nil && !seen[strings.ToLower(a)] {
			seen[strings.ToLower(a)] = true
			out = append(out, a)
		}
	}
	return out
}

func findLocal(name string, size int64) string {
	for _, d := range searchDirs() {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && (size <= 0 || st.Size() == size) {
			return p
		}
	}
	return ""
}

// assetNames lists every release file the manifest needs.
func (m *Manifest) assetNames() []ManifestPart {
	var out []ManifestPart
	for _, it := range m.Items {
		out = append(out, it.Parts...)
	}
	return out
}

// setupPlan describes what is found locally and what would be downloaded.
func setupPlan() map[string]any {
	m, err := loadManifest()
	if err != nil {
		return map[string]any{"error": "manifest: " + err.Error()}
	}
	var found, missing []map[string]any
	var missingBytes int64
	for _, p := range m.assetNames() {
		if loc := findLocal(p.Name, p.Size); loc != "" {
			found = append(found, map[string]any{"name": p.Name, "size": p.Size, "path": loc})
		} else {
			missing = append(missing, map[string]any{"name": p.Name, "size": p.Size})
			missingBytes += p.Size
		}
	}
	return map[string]any{
		"installed":     installed(),
		"appDir":        appDir,
		"found":         found,
		"missing":       missing,
		"missingBytes":  missingBytes,
		"releaseURL":    fmt.Sprintf("https://github.com/%s/releases/tag/%s", m.Repo, m.Tag),
		"tag":           m.Tag,
		"manifestReady": len(m.Items) > 0,
		"defaultTarget": defaultInstallDir(),
		"isInstalledAt": isInstallDir(),
	}
}

type setupRequest struct {
	Download     bool   `json:"download"`      // fetch missing files from the release
	Target       string `json:"target"`        // install folder; empty = this folder
	DeleteSource bool   `json:"delete_source"` // remove downloaded parts afterwards
	Shortcuts    bool   `json:"shortcuts"`
}

func runSetup(req setupRequest) {
	setup.mu.Lock()
	if setup.Running {
		setup.mu.Unlock()
		return
	}
	setup.Running, setup.Done, setup.Error, setup.Log, setup.Started = true, false, "", nil, time.Now()
	setup.mu.Unlock()

	go func() {
		err := doSetup(req)
		setup.mu.Lock()
		setup.Running = false
		if err != nil {
			setup.Error = err.Error()
		} else {
			setup.Done = true
		}
		setup.mu.Unlock()
		if err != nil {
			setup.logLine("Setup failed: %v", err)
			return
		}
		setup.logLine("Setup finished.")
	}()
}

func doSetup(req setupRequest) error {
	m, err := loadManifest()
	if err != nil {
		return err
	}
	if len(m.Items) == 0 {
		return errors.New("this build has no release manifest; put bin/ and models/ next to the exe by hand (see README)")
	}
	target := appDir
	if req.Target != "" {
		target, err = filepath.Abs(req.Target)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	dlDir := filepath.Join(target, "download")

	// 1. Locate (or download) every release file.
	paths := map[string]string{}
	for _, p := range m.assetNames() {
		loc := findLocal(p.Name, p.Size)
		if loc == "" {
			if !req.Download {
				return fmt.Errorf("%s is missing. Download it from %s and put it next to the exe, or choose Download", p.Name,
					fmt.Sprintf("https://github.com/%s/releases/tag/%s", m.Repo, m.Tag))
			}
			if err := os.MkdirAll(dlDir, 0o755); err != nil {
				return err
			}
			loc = filepath.Join(dlDir, p.Name)
			if err := downloadAsset(m, p, loc); err != nil {
				return err
			}
		} else {
			setup.logLine("Found %s", loc)
		}
		paths[p.Name] = loc
	}

	// 2. Verify, join and extract.
	for _, it := range m.Items {
		switch it.Kind {
		case "model":
			dst := filepath.Join(target, "models", it.Target)
			if ok, _ := fileMatches(dst, it.Size, it.SHA256, false); ok {
				setup.logLine("%s already installed", it.Target)
				continue
			}
			if err := joinParts(it, paths, dst); err != nil {
				return err
			}
		case "zip":
			if len(it.Parts) != 1 {
				return fmt.Errorf("zip item %s must have one part", it.ID)
			}
			src := paths[it.Parts[0].Name]
			setup.setStep("Checking "+it.Parts[0].Name, it.Parts[0].Size)
			if ok, err := fileMatches(src, it.Parts[0].Size, it.Parts[0].SHA256, true); !ok {
				return fmt.Errorf("%s failed its SHA-256 check (%v); download it again", it.Parts[0].Name, err)
			}
			if err := extractFlat(src, filepath.Join(target, it.Target)); err != nil {
				return err
			}
		}
	}

	if _, err := os.Stat(filepath.Join(target, "bin", "llama-server.exe")); err != nil {
		return errors.New("llama-server.exe was not found after extracting the engine zip")
	}
	for _, d := range []string{"data", "workspace"} {
		_ = os.MkdirAll(filepath.Join(target, d), 0o755)
	}

	if req.DeleteSource {
		for _, p := range m.assetNames() {
			loc := paths[p.Name]
			// Only delete release files, never anything inside the install itself.
			if loc != "" && !strings.HasPrefix(strings.ToLower(loc), strings.ToLower(filepath.Join(target, "models"))) {
				if os.Remove(loc) == nil {
					setup.logLine("Removed %s", loc)
				}
			}
		}
		_ = os.Remove(dlDir)
	}

	// 3. Install the exe into the target folder and add shortcuts.
	if !samePath(target, appDir) {
		dstExe := filepath.Join(target, AppName+".exe")
		if err := copyFile(exePath, dstExe); err != nil {
			return fmt.Errorf("copying %s.exe: %w", AppName, err)
		}
		if _, err := os.Stat(configPath()); err == nil {
			_ = copyFile(configPath(), filepath.Join(target, "config.json"))
		}
		setup.logLine("Installed to %s", target)
		setup.mu.Lock()
		setup.Relaunch = dstExe
		setup.mu.Unlock()
	} else {
		// Restart in place so the app picks up its real data folder.
		setup.mu.Lock()
		setup.Relaunch = exePath
		setup.mu.Unlock()
	}
	if req.Shortcuts {
		if err := createShortcuts(filepath.Join(target, AppName+".exe")); err != nil {
			setup.logLine("Could not create shortcuts: %v", err)
		} else {
			setup.logLine("Added Start menu and desktop shortcuts")
		}
	}
	return nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

type progressWriter struct{}

func (progressWriter) Write(p []byte) (int, error) {
	setup.add(int64(len(p)))
	return len(p), nil
}

// fileMatches checks size and (optionally) SHA-256.
func fileMatches(path string, size int64, sum string, hash bool) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if size > 0 && st.Size() != size {
		return false, fmt.Errorf("size %d, expected %d", st.Size(), size)
	}
	if !hash && size > 0 {
		// A size match on an installed model is enough to skip re-joining;
		// it was hash-checked when it was written.
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, progressWriter{}), f); err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, sum) {
		return false, fmt.Errorf("sha256 %s, expected %s", got, sum)
	}
	return true, nil
}

// joinParts concatenates the parts in order, checking each part's hash and the
// hash of the whole file, then moves it into place.
func joinParts(it ManifestItem, paths map[string]string, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	whole := sha256.New()
	setup.setStep("Assembling "+it.Target, it.Size)
	setup.logLine("Assembling %s from %d part(s)", it.Target, len(it.Parts))
	for _, p := range it.Parts {
		in, err := os.Open(paths[p.Name])
		if err != nil {
			out.Close()
			return err
		}
		ph := sha256.New()
		_, err = io.Copy(io.MultiWriter(out, whole, ph, progressWriter{}), in)
		in.Close()
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		if got := hex.EncodeToString(ph.Sum(nil)); !strings.EqualFold(got, p.SHA256) {
			out.Close()
			os.Remove(tmp)
			return fmt.Errorf("%s is damaged (SHA-256 mismatch); delete it and download it again", p.Name)
		}
		setup.logLine("  %s OK", p.Name)
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(whole.Sum(nil)); !strings.EqualFold(got, it.SHA256) {
		os.Remove(tmp)
		return fmt.Errorf("%s: SHA-256 of the joined file does not match", it.Target)
	}
	setup.logLine("%s verified (SHA-256 %s…)", it.Target, it.SHA256[:12])
	_ = os.Remove(dst)
	return os.Rename(tmp, dst)
}

// extractFlat unpacks a zip into dir, dropping any folder structure (the
// llama.cpp zips keep every file at the top level anyway).
func extractFlat(src, dir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var total int64
	for _, f := range zr.File {
		total += int64(f.UncompressedSize64)
	}
	setup.setStep("Extracting "+filepath.Base(src), total)
	n := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(filepath.FromSlash(strings.ReplaceAll(f.Name, "\\", "/")))
		if name == "." || name == ".." || name == "" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, name)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(io.MultiWriter(out, progressWriter{}), rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		n++
	}
	setup.logLine("Extracted %d files from %s", n, filepath.Base(src))
	return nil
}

// downloadAsset fetches one release file, resuming a previous partial
// download, and checks its SHA-256.
func downloadAsset(m *Manifest, p ManifestPart, dst string) error {
	u := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", m.Repo, m.Tag, p.Name)
	tmp := dst + ".download"
	var have int64
	if st, err := os.Stat(tmp); err == nil {
		have = st.Size()
	}
	setup.setStep("Downloading "+p.Name, p.Size)
	setup.add(have)
	setup.logLine("Downloading %s (%s)", p.Name, humanBytes(p.Size))
	for attempt := 1; ; attempt++ {
		err := func() error {
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("User-Agent", AppName+"/"+Version)
			if have > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
			}
			resp, err := (&http.Client{}).Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			flags := os.O_CREATE | os.O_WRONLY
			switch resp.StatusCode {
			case http.StatusPartialContent:
				flags |= os.O_APPEND
			case http.StatusOK:
				flags |= os.O_TRUNC
				setup.setStep("Downloading "+p.Name, p.Size)
				have = 0
			case http.StatusRequestedRangeNotSatisfiable:
				return nil
			default:
				return fmt.Errorf("HTTP %d for %s", resp.StatusCode, u)
			}
			f, err := os.OpenFile(tmp, flags, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(io.MultiWriter(f, progressWriter{}), resp.Body)
			have += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return err
		}()
		if err == nil {
			break
		}
		if attempt >= 6 {
			return fmt.Errorf("downloading %s: %w", p.Name, err)
		}
		setup.logLine("  retrying after error: %v", err)
		time.Sleep(time.Duration(attempt*2) * time.Second)
	}
	setup.setStep("Verifying "+p.Name, p.Size)
	if ok, err := fileMatches(tmp, p.Size, p.SHA256, true); !ok {
		os.Remove(tmp)
		return fmt.Errorf("%s failed verification: %v", p.Name, err)
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o755)
	if st, err := in.Stat(); err == nil {
		mode = st.Mode().Perm() | 0o600
	}
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Rename(dst+".tmp", dst)
}

func defaultInstallDir() string {
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "Programs", AppName)
	}
	return appDir
}

// isInstallDir is true when the exe already runs from its install folder.
func isInstallDir() bool {
	return samePath(appDir, defaultInstallDir()) || installed()
}
