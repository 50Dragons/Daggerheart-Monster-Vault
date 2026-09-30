package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type sourceDef struct {
	Key    string
	Label  string
	Folder string
}

var sources = []sourceDef{
	{Key: "custom", Label: "Custom", Folder: "Custom monsters"},
	{Key: "astrellon", Label: "Astrellon Monsters", Folder: "Astrellon monsters"},
	{Key: "astrellonepic", Label: "Astrellon Epic Creatures", Folder: "Astrellon Epic creatures"},
	{Key: "core", Label: "Core", Folder: "Core"},
	{Key: "hopefear", Label: "Hope & Fear", Folder: "Hope and Fear"},
	{Key: "envsocial", Label: "Environment & Social", Folder: "Environment and Social"},
}

const engineVersion = "1.2"

type frontendManifest struct {
	App             string `json:"app"`
	FrontendVersion string `json:"frontendVersion"`
	RequiredEngine  string `json:"requiredEngine"`
}

type appServer struct {
	baseDir  string
	token    string
	html     []byte
	srv      *http.Server
	lastPing atomic.Int64
	seenPing atomic.Bool
}

type apiError struct {
	Error string `json:"error"`
}

type saveRequest struct {
	Filename  string          `json:"filename"`
	Data      json.RawMessage `json:"data"`
	Overwrite bool            `json:"overwrite"`
}

type folderRequest struct {
	SourceKey string `json:"sourceKey"`
}

func main() {
	baseDir, err := applicationDir()
	if err != nil {
		writeStartupError("Could not determine application folder: " + err.Error())
		return
	}
	if err := ensureFolders(baseDir); err != nil {
		writeStartupError("Could not initialize application folders: " + err.Error())
		return
	}

	token, err := makeToken()
	if err != nil {
		writeStartupError("Could not generate a security token: " + err.Error())
		return
	}

	html, frontendVersion, err := loadFrontend(baseDir, token)
	if err != nil {
		writeStartupError("Could not load interface: " + err.Error())
		return
	}
	_ = frontendVersion

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		writeStartupError("Could not start local application service: " + err.Error())
		return
	}

	a := &appServer{baseDir: baseDir, token: token, html: html}
	mux := http.NewServeMux()
	mux.Handle("/frontend/", a.frontendFileHandler())
	mux.HandleFunc("/monster-image", a.handleMonsterImage)
	mux.HandleFunc("/api/engine-info", a.handleEngineInfo)
	mux.HandleFunc("/api/ping", a.handlePing)
	mux.HandleFunc("/api/monsters", a.handleMonsters)
	mux.HandleFunc("/api/saves", a.handleSaves)
	mux.HandleFunc("/api/save", a.handleLoadSave)
	mux.HandleFunc("/api/save-encounter", a.handleSaveEncounter)
	mux.HandleFunc("/api/save-monster", a.handleSaveMonster)
	mux.HandleFunc("/api/save-special", a.handleSaveSpecial)
	mux.HandleFunc("/api/open-folder", a.handleOpenFolder)
	mux.HandleFunc("/api/shutdown", a.handleShutdown)
	mux.HandleFunc("/", a.handleRoot)

	a.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	addr := listener.Addr().String()
	url := "http://" + addr + "/"

	serveDone := make(chan error, 1)
	go func() {
		err := a.srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	if os.Getenv("ASTRELLON_NO_BROWSER") == "" {
		if err := launchAppWindow(url); err != nil {
			appendLog(baseDir, "Browser launch warning: "+err.Error())
			_ = launchFallbackBrowser(url)
		}
	} else {
		// Useful for development and automated local checks.
		fmt.Println(url)
	}

	// Exit after the UI has connected and then disappears. This prevents a
	// hidden localhost process from lingering after the app window is closed.
	go a.monitorIdle()

	if testSeconds, _ := strconv.Atoi(os.Getenv("ASTRELLON_TEST_EXIT_SECONDS")); testSeconds > 0 {
		go func() {
			time.Sleep(time.Duration(testSeconds) * time.Second)
			_ = a.srv.Close()
		}()
	}

	if err := <-serveDone; err != nil {
		appendLog(baseDir, "Server error: "+err.Error())
	}
}

func compareVersions(a, b string) int {
	parse := func(s string) []int {
		parts := strings.Split(strings.TrimSpace(s), ".")
		out := make([]int, len(parts))
		for i, p := range parts {
			out[i], _ = strconv.Atoi(p)
		}
		return out
	}
	aa, bb := parse(a), parse(b)
	n := len(aa)
	if len(bb) > n {
		n = len(bb)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(aa) {
			av = aa[i]
		}
		if i < len(bb) {
			bv = bb[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func loadFrontend(baseDir, token string) ([]byte, string, error) {
	frontendVersion := "1.7653"
	externalHTML := filepath.Join(baseDir, "Frontend", "index.html")
	externalManifest := filepath.Join(baseDir, "Frontend", "manifest.json")
	if b, err := os.ReadFile(externalManifest); err == nil {
		var m frontendManifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, "", fmt.Errorf("invalid Frontend\\manifest.json: %w", err)
		}
		if strings.TrimSpace(m.FrontendVersion) != "" {
			frontendVersion = strings.TrimSpace(m.FrontendVersion)
		}
		if strings.TrimSpace(m.RequiredEngine) != "" && compareVersions(engineVersion, m.RequiredEngine) < 0 {
			return nil, "", fmt.Errorf("Frontend %s requires Engine %s or newer. This executable is Engine %s.", frontendVersion, m.RequiredEngine, engineVersion)
		}
		if b, err := os.ReadFile(externalHTML); err == nil {
			s := string(b)
			s = strings.ReplaceAll(s, "__ASTRELLON_TOKEN__", token)
			s = strings.ReplaceAll(s, "__ASTRELLON_FRONTEND_VERSION__", frontendVersion)
			s = strings.ReplaceAll(s, "__ASTRELLON_ENGINE_VERSION__", engineVersion)
			return []byte(s), frontendVersion, nil
		}
	}
	rawHTML, err := webFS.ReadFile("web/index.html")
	if err != nil {
		return nil, "", err
	}
	s := string(rawHTML)
	s = strings.ReplaceAll(s, "__ASTRELLON_TOKEN__", token)
	s = strings.ReplaceAll(s, "__ASTRELLON_FRONTEND_VERSION__", frontendVersion)
	s = strings.ReplaceAll(s, "__ASTRELLON_ENGINE_VERSION__", engineVersion)
	return []byte(s), frontendVersion, nil
}

func (a *appServer) frontendFileHandler() http.Handler {
	root := filepath.Join(a.baseDir, "Frontend")
	fs := http.StripPrefix("/frontend/", http.FileServer(http.Dir(root)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		if !a.validHost(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fs.ServeHTTP(w, r)
	})
}

func (a *appServer) handleEngineInfo(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engineVersion": engineVersion})
}

func sourceByKey(key string) (sourceDef, bool) {
	for _, src := range sources {
		if src.Key == key {
			return src, true
		}
	}
	return sourceDef{}, false
}

func siblingImagePath(folder, jsonName string) string {
	stem := strings.TrimSuffix(jsonName, filepath.Ext(jsonName))
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp"} {
		candidate := stem + ext
		if st, err := os.Stat(filepath.Join(folder, candidate)); err == nil && !st.IsDir() {
			return candidate
		}
		// tolerate uppercase/mixed-case extension/file names
		entries, _ := os.ReadDir(folder)
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(e.Name(), candidate) {
				return e.Name()
			}
		}
	}
	return ""
}

func (a *appServer) monsterImageURL(sourceKey, imageFile string) string {
	return "/monster-image?sourceKey=" + url.QueryEscape(sourceKey) + "&file=" + url.QueryEscape(imageFile)
}

func (a *appServer) handleMonsterImage(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.validHost(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	src, ok := sourceByKey(r.URL.Query().Get("sourceKey"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := r.URL.Query().Get("file")
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\:*?"<>|`) {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".bmp": true}
	if !allowed[ext] {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(a.baseDir, src.Folder, name)
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, full)
}

func (a *appServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go func() { time.Sleep(150 * time.Millisecond); _ = a.srv.Close() }()
}

func applicationDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("ASTRELLON_APP_DIR")); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func ensureFolders(base string) error {
	for _, folder := range []string{"Custom monsters", "Astrellon monsters", "Astrellon Epic creatures", "Core", "Hope and Fear", "Environment and Social", "Saves"} {
		if err := os.MkdirAll(filepath.Join(base, folder), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func makeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *appServer) validHost(r *http.Request) bool {
	host := strings.ToLower(r.Host)
	return strings.HasPrefix(host, "127.0.0.1:") || host == "127.0.0.1" || strings.HasPrefix(host, "localhost:") || host == "localhost"
}

func (a *appServer) authorize(w http.ResponseWriter, r *http.Request) bool {
	if !a.validHost(r) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "Invalid host."})
		return false
	}
	if r.Header.Get("X-Astrellon-Token") != a.token {
		writeJSON(w, http.StatusForbidden, apiError{Error: "Unauthorized local request."})
		return false
	}
	return true
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
}

func (a *appServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if !a.validHost(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(a.html)
}

func (a *appServer) handlePing(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodGet || !a.authorize(w, r) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	a.seenPing.Store(true)
	a.lastPing.Store(time.Now().UnixNano())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *appServer) monitorIdle() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !a.seenPing.Load() {
			continue
		}
		last := time.Unix(0, a.lastPing.Load())
		if time.Since(last) > 45*time.Second {
			_ = a.srv.Close()
			return
		}
	}
}

func (a *appServer) handleMonsters(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}

	monsters := make([]map[string]any, 0)
	errs := make([]string, 0)
	counts := map[string]int{}
	sourcePaths := map[string]string{"saves": filepath.Join(a.baseDir, "Saves")}

	for _, src := range sources {
		folder := filepath.Join(a.baseDir, src.Folder)
		sourcePaths[src.Key] = folder
		if err := os.MkdirAll(folder, 0o755); err != nil {
			errs = append(errs, src.Label+": "+err.Error())
			continue
		}
		entries, err := os.ReadDir(folder)
		if err != nil {
			errs = append(errs, src.Label+": "+err.Error())
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
				continue
			}
			full := filepath.Join(folder, entry.Name())
			b, err := os.ReadFile(full)
			if err != nil {
				errs = append(errs, src.Label+" / "+entry.Name()+": "+err.Error())
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal(b, &obj); err != nil {
				errs = append(errs, src.Label+" / "+entry.Name()+": "+err.Error())
				continue
			}
			if strings.TrimSpace(toString(obj["name"])) == "" {
				obj["name"] = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			}
			obj["_file"] = entry.Name()
			obj["_sourceKey"] = src.Key
			obj["_sourceLabel"] = src.Label
			if imageFile := siblingImagePath(folder, entry.Name()); imageFile != "" {
				obj["_imageFile"] = imageFile
				obj["_imageUrl"] = a.monsterImageURL(src.Key, imageFile)
			}
			monsters = append(monsters, obj)
			counts[src.Key]++
		}
	}

	sort.Slice(monsters, func(i, j int) bool {
		return strings.ToLower(toString(monsters[i]["name"])) < strings.ToLower(toString(monsters[j]["name"]))
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"monsters":    monsters,
		"errors":      errs,
		"counts":      counts,
		"sourcePaths": sourcePaths,
	})
}

func (a *appServer) handleSaves(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	folder := filepath.Join(a.baseDir, "Saves")
	entries, err := os.ReadDir(folder)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	files := make([]string, 0)
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			files = append(files, e.Name())
		}
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i]) < strings.ToLower(files[j]) })
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (a *appServer) handleLoadSave(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	name := r.URL.Query().Get("name")
	if err := validateJSONFilename(name); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	full := filepath.Join(a.baseDir, "Saves", name)
	b, err := os.ReadFile(full)
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, apiError{Error: err.Error()})
		return
	}
	var data any
	if err := json.Unmarshal(b, &data); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Invalid encounter JSON: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a *appServer) handleSaveEncounter(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	var req saveRequest
	if err := decodeLimitedJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	if err := validateJSONFilename(req.Filename); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Data, &obj); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Invalid encounter data: " + err.Error()})
		return
	}
	if toString(obj["fileType"]) != "Astrellon Monster Vault Encounter" {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Encounter fileType is missing or invalid."})
		return
	}
	full := filepath.Join(a.baseDir, "Saves", req.Filename)
	if !req.Overwrite {
		if _, err := os.Stat(full); err == nil {
			writeJSON(w, http.StatusConflict, apiError{Error: "A save with that name already exists."})
			return
		}
	}
	if err := writeJSONAtomic(full, obj); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": full})
}

func (a *appServer) handleSaveMonster(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	var req saveRequest
	if err := decodeLimitedJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	if err := validateJSONFilename(req.Filename); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Data, &obj); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Invalid monster data: " + err.Error()})
		return
	}
	if err := validateMonster(obj); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	full := filepath.Join(a.baseDir, "Custom monsters", req.Filename)
	if !req.Overwrite {
		if _, err := os.Stat(full); err == nil {
			writeJSON(w, http.StatusConflict, apiError{Error: "A custom monster with that filename already exists."})
			return
		}
	}
	if err := writeJSONAtomic(full, obj); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": full})
}

func (a *appServer) handleSaveSpecial(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	var req saveRequest
	if err := decodeLimitedJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	if err := validateJSONFilename(req.Filename); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Data, &obj); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Invalid special card data: " + err.Error()})
		return
	}
	if err := validateSpecialCard(obj); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	full := filepath.Join(a.baseDir, "Environment and Social", req.Filename)
	if !req.Overwrite {
		if _, err := os.Stat(full); err == nil {
			writeJSON(w, http.StatusConflict, apiError{Error: "A special card with that filename already exists."})
			return
		}
	}
	if err := writeJSONAtomic(full, obj); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": full})
}

func validateSpecialCard(m map[string]any) error {
	for _, k := range []string{"id", "name", "type", "description"} {
		if strings.TrimSpace(toString(m[k])) == "" {
			return fmt.Errorf("required special-card field %q is missing", k)
		}
	}
	tier, ok := numberInt(m["tier"])
	if !ok || tier < 1 || tier > 4 {
		return errors.New("tier must be 1, 2, 3, or 4")
	}
	level, ok := numberInt(m["level"])
	if !ok || level != tier {
		return errors.New("level must match tier")
	}
	diff, ok := numberInt(m["difficulty"])
	if !ok || diff < 1 {
		return errors.New("difficulty must be a positive number")
	}
	imp, ok := m["impulses"].([]any)
	if !ok || len(imp) == 0 {
		return errors.New("at least one impulse is required")
	}
	features, ok := m["features"].([]any)
	if !ok || len(features) == 0 {
		return errors.New("at least one feature is required")
	}
	return nil
}

func (a *appServer) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.authorize(w, r) {
		return
	}
	var req folderRequest
	if err := decodeLimitedJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	folder := ""
	for _, src := range sources {
		if req.SourceKey == src.Key {
			folder = filepath.Join(a.baseDir, src.Folder)
			break
		}
	}
	if folder == "" {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Unknown source folder."})
		return
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	if err := openFolder(folder); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": folder})
}

func decodeLimitedJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	lr := io.LimitReader(r.Body, 4<<20)
	dec := json.NewDecoder(lr)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid request JSON: %w", err)
	}
	return nil
}

func validateJSONFilename(name string) error {
	if name == "" || len(name) > 180 {
		return errors.New("invalid JSON filename")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\\:*?"<>|`) {
		return errors.New("filename contains invalid characters")
	}
	if !strings.EqualFold(filepath.Ext(name), ".json") {
		return errors.New("filename must end in .json")
	}
	if name == ".json" || strings.HasSuffix(name, " .json") {
		return errors.New("invalid JSON filename")
	}
	return nil
}

func validateMonster(m map[string]any) error {
	requiredStrings := []string{"id", "name", "type", "role", "description"}
	for _, k := range requiredStrings {
		if strings.TrimSpace(toString(m[k])) == "" {
			return fmt.Errorf("required monster field %q is missing", k)
		}
	}
	tier, ok := numberInt(m["tier"])
	if !ok || tier < 1 || tier > 4 {
		return errors.New("tier must be 1, 2, 3, or 4")
	}
	level, ok := numberInt(m["level"])
	if !ok || level != tier {
		return errors.New("level must match tier (L1-L4 = Tier 1-4)")
	}
	difficulty, ok := numberInt(m["difficulty"])
	if !ok || difficulty < 1 {
		return errors.New("difficulty must be a positive number")
	}
	hp, ok := numberInt(m["hp"])
	if !ok || hp < 1 {
		return errors.New("hp must be at least 1")
	}
	stress, ok := numberInt(m["stress"])
	if !ok || stress < 0 {
		return errors.New("stress cannot be negative")
	}
	th, ok := m["thresholds"].(map[string]any)
	if !ok {
		return errors.New("thresholds are missing")
	}
	major, ok1 := numberInt(th["major"])
	severe, ok2 := numberInt(th["severe"])
	if !ok1 || !ok2 || major < 0 || severe < major {
		return errors.New("thresholds must contain valid major/severe values")
	}
	attacks, ok := m["attacks"].([]any)
	if !ok || len(attacks) == 0 {
		return errors.New("at least one attack is required")
	}
	motives, ok := m["motivesTactics"].([]any)
	if !ok || len(motives) == 0 {
		return errors.New("at least one Motive & Tactic is required")
	}
	return nil
}

func numberInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), x == float64(int(x))
	case int:
		return x, true
	case json.Number:
		i, err := strconv.Atoi(x.String())
		return i, err == nil
	default:
		return 0, false
	}
}

func writeJSONAtomic(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\r', '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func launchAppWindow(url string) error {
	if runtime.GOOS != "windows" {
		return launchFallbackBrowser(url)
	}
	candidates := []string{}
	if p := os.Getenv("PROGRAMFILES(X86)"); p != "" {
		candidates = append(candidates, filepath.Join(p, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
	if p := os.Getenv("PROGRAMFILES"); p != "" {
		candidates = append(candidates, filepath.Join(p, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
	if p := os.Getenv("LOCALAPPDATA"); p != "" {
		candidates = append(candidates, filepath.Join(p, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
	for _, edge := range candidates {
		if _, err := os.Stat(edge); err == nil {
			cmd := exec.Command(edge, "--app="+url, "--start-maximized", "--window-position=0,0", "--no-first-run")
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	return errors.New("Microsoft Edge was not found in its standard locations")
}

func launchFallbackBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func openFolder(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer.exe", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func appendLog(baseDir, message string) {
	path := filepath.Join(baseDir, "AstrellonMonsterVault.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s  %s\r\n", time.Now().Format(time.RFC3339), message)
}

func writeStartupError(message string) {
	// GUI builds have no console. Write a deterministic error file in the
	// current working folder if possible.
	_ = os.WriteFile("AstrellonMonsterVault-startup-error.txt", []byte(message+"\r\n"), 0o644)
	log.Print(message)
}

func init() {
	// Register JSON explicitly on systems with unusual MIME databases.
	_ = mime.AddExtensionType(".json", "application/json")
}
