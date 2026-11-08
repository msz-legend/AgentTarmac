package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// RecentCWDPath is the server-side memory of recently used dispatch working
// directories, shared between the PC shell and the Web console. Stored as a
// JSON array of strings in <data>/recent_cwd.json.
const recentCWDMax = 8

type fsBrowser struct {
	mu         sync.Mutex
	recent     []string
	recentPath string
}

// newFsBrowser loads (or creates) the recent-directory memory.
func newFsBrowser(dataDir string) *fsBrowser {
	fb := &fsBrowser{recentPath: filepath.Join(dataDir, "recent_cwd.json")}
	_ = fb.load()
	return fb
}

func (fb *fsBrowser) load() error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	raw, err := os.ReadFile(fb.recentPath)
	if err != nil {
		if os.IsNotExist(err) {
			fb.recent = nil
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		fb.recent = nil
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return err
	}
	fb.recent = arr
	return nil
}

func (fb *fsBrowser) save() error {
	raw, err := json.Marshal(fb.recent)
	if err != nil {
		return err
	}
	// Atomic write: write to tmp then rename.
	tmp := fb.recentPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fb.recentPath)
}

// add appends path to the front of the recent list (dedup, capped).
func (fb *fsBrowser) add(path string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	clean := filepath.Clean(path)
	if clean == "" {
		return
	}
	out := make([]string, 0, len(fb.recent)+1)
	out = append(out, clean)
	for _, p := range fb.recent {
		if p == clean {
			continue
		}
		out = append(out, p)
	}
	if len(out) > recentCWDMax {
		out = out[:recentCWDMax]
	}
	fb.recent = out
	_ = fb.save()
}

func (fb *fsBrowser) list() []string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make([]string, len(fb.recent))
	copy(out, fb.recent)
	return out
}

// fsListEntry describes one browsable directory entry.
type fsListEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

// handleFSRoots returns the browsable roots (Windows drive letters, or "/").
func (s *Server) handleFSRoots(w http.ResponseWriter, r *http.Request) {
	var roots []string
	if isWindows() {
		for _, letter := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
			root := string(letter) + `:\`
			if _, err := os.Stat(root); err == nil {
				roots = append(roots, root)
			}
		}
	} else {
		roots = []string{"/"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

// handleFSList lists the immediate child directories of path.
func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	info, err := os.Stat(clean)
	if err != nil {
		writeError(w, http.StatusBadRequest, "path does not exist: "+err.Error())
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is not a directory")
		return
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read dir failed: "+err.Error())
		return
	}
	var out []fsListEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "$") {
			continue // skip hidden/system dirs
		}
		out = append(out, fsListEntry{
			Name:  e.Name(),
			Path:  filepath.Join(clean, e.Name()),
			IsDir: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	parent := ""
	if clean != filepath.VolumeName(clean) {
		parent = filepath.Dir(clean)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    clean,
		"parent":  parent,
		"entries": out,
	})
}

func (s *Server) handleFSRecentList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"recent": s.fsBrowser.list()})
}

func (s *Server) handleFSRecentAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	clean := filepath.Clean(body.Path)
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "not a directory: "+clean)
		return
	}
	s.fsBrowser.add(clean)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recent": s.fsBrowser.list()})
}

func isWindows() bool { return os.PathSeparator == '\\' }
