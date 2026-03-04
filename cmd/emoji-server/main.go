package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	ListenAddr string `json:"listen_addr"`
	DataDir    string `json:"data_dir"`
	PublicKey  string `json:"public_key"`
	UIKey      string `json:"ui_key"`
	BaseURL    string `json:"base_url"`
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = ":8080"
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		cfg.DataDir = "./emojis"
	}
	cfg.PublicKey = strings.TrimSpace(cfg.PublicKey)
	cfg.UIKey = strings.TrimSpace(cfg.UIKey)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	if cfg.PublicKey == "" {
		return Config{}, errors.New("config.public_key 不能为空")
	}
	if cfg.UIKey == "" {
		return Config{}, errors.New("config.ui_key 不能为空")
	}
	return cfg, nil
}

type FileInfo struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	DisplayName string    `json:"display_name,omitempty"`
	Note        string    `json:"note,omitempty"`
}

type FileStore struct {
	dir string
}

func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) List(ctx context.Context) ([]FileInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, FileInfo{
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

type EmojiMeta struct {
	DisplayName string    `json:"display_name,omitempty"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type MetaStore struct {
	mu   sync.Mutex
	path string
	m    map[string]EmojiMeta
}

func NewMetaStore(dataDir string) (*MetaStore, error) {
	ms := &MetaStore{
		path: filepath.Join(dataDir, ".emoji-meta.json"),
		m:    make(map[string]EmojiMeta),
	}
	b, err := os.ReadFile(ms.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ms, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return ms, nil
	}
	if err := json.Unmarshal(b, &ms.m); err != nil {
		return nil, err
	}
	return ms, nil
}

func (s *MetaStore) Get(name string) (EmojiMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[name]
	return v, ok
}

func (s *MetaStore) Set(name, displayName, note string) error {
	displayName = strings.TrimSpace(displayName)
	note = strings.TrimSpace(note)

	s.mu.Lock()
	defer s.mu.Unlock()

	if displayName == "" && note == "" {
		delete(s.m, name)
		return s.saveLocked()
	}
	now := time.Now()
	prev, ok := s.m[name]
	if !ok {
		prev.CreatedAt = now
	}
	prev.DisplayName = displayName
	prev.Note = note
	prev.UpdatedAt = now
	s.m[name] = prev
	return s.saveLocked()
}

func (s *MetaStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[name]; !ok {
		return nil
	}
	delete(s.m, name)
	return s.saveLocked()
}

func (s *MetaStore) Rename(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if oldName == newName {
		return nil
	}
	v, ok := s.m[oldName]
	if !ok {
		return nil
	}
	delete(s.m, oldName)
	s.m[newName] = v
	return s.saveLocked()
}

func (s *MetaStore) saveLocked() error {
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".meta-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return renameOver(tmpName, s.path)
}

func (s *FileStore) Open(name string) (string, *os.File, error) {
	safe, err := sanitizeFilename(name)
	if err != nil {
		return "", nil, err
	}
	full := filepath.Join(s.dir, safe)
	f, err := os.Open(full)
	if err != nil {
		return "", nil, err
	}
	return full, f, nil
}

func (s *FileStore) Save(name string, r io.Reader) (string, error) {
	safe, err := sanitizeFilename(name)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(s.dir, safe)

	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := renameOver(tmpName, dst); err != nil {
		return "", err
	}
	return safe, nil
}

func renameOver(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

func (s *FileStore) Delete(name string) error {
	safe, err := sanitizeFilename(name)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.dir, safe))
}

func (s *FileStore) Rename(oldName, newName string) (string, error) {
	oldSafe, err := sanitizeFilename(oldName)
	if err != nil {
		return "", err
	}
	newSafe, err := sanitizeFilename(newName)
	if err != nil {
		return "", err
	}
	if oldSafe == newSafe {
		return newSafe, nil
	}

	oldPath := filepath.Join(s.dir, oldSafe)
	newPath := filepath.Join(s.dir, newSafe)

	if _, err := os.Stat(oldPath); err != nil {
		return "", err
	}
	if _, err := os.Stat(newPath); err == nil {
		return "", os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.Rename(oldPath, newPath); err == nil {
		return newSafe, nil
	}

	if strings.EqualFold(oldSafe, newSafe) {
		tmp, err := os.CreateTemp(s.dir, ".rename-*")
		if err != nil {
			return "", err
		}
		tmpName := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(tmpName)

		if err := os.Rename(oldPath, tmpName); err != nil {
			return "", err
		}
		if err := os.Rename(tmpName, newPath); err != nil {
			_ = os.Rename(tmpName, oldPath)
			return "", err
		}
		return newSafe, nil
	}

	return "", errors.New("重命名失败")
}

func sanitizeFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", errors.New("文件名无效")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return "", errors.New("文件名无效")
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.New("不允许隐藏文件名")
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", errors.New("文件名不能以点或空格结尾")
	}
	if utf8.RuneCountInString(name) > 200 {
		return "", errors.New("文件名过长")
	}
	for _, r := range name {
		if r == 0 || r < 32 {
			return "", errors.New("文件名包含非法字符")
		}
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return "", errors.New("文件名包含非法字符")
		}
	}
	if isWindowsReservedName(name) {
		return "", errors.New("文件名为系统保留字")
	}
	return name, nil
}

func isWindowsReservedName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	base = strings.ToUpper(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	default:
		return false
	}
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
	}
}

func (s *SessionStore) New() (string, time.Time, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(s.ttl)
	s.mu.Lock()
	s.sessions[token] = exp
	s.mu.Unlock()
	return token, exp, nil
}

func (s *SessionStore) Valid(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *SessionStore) Delete(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *SessionStore) GC() {
	now := time.Now()
	s.mu.Lock()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
	s.mu.Unlock()
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const adminCookieName = "emoji_admin"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("读取 config.json 失败：%v", err)
	}

	store, err := NewFileStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化资源目录失败：%v", err)
	}

	meta, err := NewMetaStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化元数据失败：%v", err)
	}

	sessions := NewSessionStore(12 * time.Hour)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			sessions.GC()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusFound)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	// Public emoji access: /e/{publicKey}/{filename}
	mux.HandleFunc("/e/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 4 || parts[1] != "e" {
			http.NotFound(w, r)
			return
		}
		key := parts[2]
		if v, err := url.PathUnescape(key); err == nil {
			key = v
		}
		name := parts[3]
		if v, err := url.PathUnescape(name); err == nil {
			name = v
		}
		if key != cfg.PublicKey {
			http.NotFound(w, r)
			return
		}
		full, f, err := store.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		stat, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}

		ext := strings.ToLower(filepath.Ext(full))
		if ct := mime.TypeByExtension(ext); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
	})

	// Admin pages
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin" {
			http.NotFound(w, r)
			return
		}
		if !isAdmin(r, sessions) {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		renderAdmin(w, cfg)
	})
	mux.HandleFunc("/admin/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			renderLogin(w, "")
		case http.MethodPost:
			if err := r.ParseForm(); err != nil {
				renderLogin(w, "请求格式错误")
				return
			}
			key := strings.TrimSpace(r.FormValue("key"))
			if key == "" || key != cfg.UIKey {
				renderLogin(w, "key 不正确")
				return
			}
			token, exp, err := sessions.New()
			if err != nil {
				renderLogin(w, "创建会话失败")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     adminCookieName,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				Expires:  exp,
			})
			http.Redirect(w, r, "/admin", http.StatusFound)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		c, err := r.Cookie(adminCookieName)
		if err == nil {
			sessions.Delete(c.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     adminCookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
		})
		http.Redirect(w, r, "/admin/login", http.StatusFound)
	})

	// Admin APIs
	mux.HandleFunc("/api/admin/files", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		files, err := store.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for i := range files {
			if m, ok := meta.Get(files[i].Name); ok {
				files[i].DisplayName = m.DisplayName
				files[i].Note = m.Note
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
	}))

	mux.HandleFunc("/api/admin/upload", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 100<<20)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "解析上传失败（文件过大或格式错误）"})
			return
		}
		files := r.MultipartForm.File["file"]
		if len(files) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 file"})
			return
		}
		customName := strings.TrimSpace(r.FormValue("name"))
		if len(files) > 1 && customName != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "多文件上传不支持自定义 name"})
			return
		}
		saved := make([]string, 0, len(files))
		for _, header := range files {
			f, err := header.Open()
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取上传文件失败"})
				return
			}
			name := header.Filename
			if customName != "" {
				name = customName
			}
			out, err := store.Save(name, f)
			_ = f.Close()
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			saved = append(saved, out)
		}
		resp := map[string]any{"names": saved}
		if len(saved) == 1 {
			resp["name"] = saved[0]
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/admin/meta/set", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Note        string `json:"note"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		safe, err := sanitizeFilename(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		full := filepath.Join(cfg.DataDir, safe)
		if _, err := os.Stat(full); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取文件失败"})
			return
		}
		if err := meta.Set(safe, req.DisplayName, req.Note); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存元数据失败"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/admin/rename", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			OldName string `json:"old_name"`
			NewName string `json:"new_name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		oldSafe, err := sanitizeFilename(req.OldName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		newSafe, err := sanitizeFilename(req.NewName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if oldSafe == newSafe {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": newSafe})
			return
		}
		out, err := store.Rename(oldSafe, newSafe)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
				return
			}
			if errors.Is(err, os.ErrExist) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "目标文件已存在"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		_ = meta.Rename(oldSafe, out)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": out})
	}))

	mux.HandleFunc("/api/admin/delete", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		if err := store.Delete(req.Name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if safe, err := sanitizeFilename(req.Name); err == nil {
			_ = meta.Delete(safe)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("监听失败：%v", err)
	}
	log.Printf("emoji-server listening on %s", ln.Addr().String())
	log.Printf("admin: http://%s/admin", hostForLog(ln.Addr()))
	log.Printf("public: http://%s/e/%s/<filename>", hostForLog(ln.Addr()), cfg.PublicKey)
	log.Fatal(srv.Serve(ln))
}

func hostForLog(addr net.Addr) string {
	s := addr.String()
	if strings.HasPrefix(s, ":") {
		return "localhost" + s
	}
	if strings.HasPrefix(s, "0.0.0.0:") {
		return "localhost" + strings.TrimPrefix(s, "0.0.0.0")
	}
	if strings.HasPrefix(s, "[::]:") {
		return "localhost" + strings.TrimPrefix(s, "[::]")
	}
	return s
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Truncate(time.Millisecond))
	})
}

func isAdmin(r *http.Request, sessions *SessionStore) bool {
	c, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	return sessions.Valid(c.Value)
}

func requireAdmin(sessions *SessionStore, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, sessions) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var loginTpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>emoji-server 登录</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; padding: 24px; }
    .card { max-width: 460px; margin: 8vh auto 0; padding: 18px; border: 1px solid #4444; border-radius: 12px; }
    h1 { font-size: 18px; margin: 0 0 12px; }
    label { display: block; font-size: 13px; margin: 10px 0 6px; opacity: 0.9; }
    input { width: 100%; padding: 10px 12px; border-radius: 10px; border: 1px solid #4446; background: transparent; }
    button { margin-top: 12px; width: 100%; padding: 10px 12px; border-radius: 10px; border: 0; background: #4f46e5; color: white; font-weight: 600; cursor: pointer; }
    .err { margin-top: 10px; color: #ef4444; font-size: 13px; }
    .hint { margin-top: 10px; font-size: 12px; opacity: 0.8; }
  </style>
</head>
<body>
  <div class="card">
    <h1>emoji-server 管理登录</h1>
    <form method="post" action="/admin/login">
      <label for="key">ui-key</label>
      <input id="key" name="key" type="password" autocomplete="current-password" required>
      <button type="submit">登录</button>
    </form>
    {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
    <div class="hint">提示：` + "`config.json`" + ` 里配置的 ` + "`ui_key`" + `。</div>
  </div>
</body>
</html>`))

func renderLogin(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTpl.Execute(w, struct {
		Error string
	}{Error: errMsg})
}

var adminTpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>emoji-server 管理</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; padding: 18px; }
    header { display: flex; gap: 12px; align-items: center; justify-content: space-between; flex-wrap: wrap; margin-bottom: 16px; }
    h1 { font-size: 18px; margin: 0; }
    .row { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
    .pill { font-size: 12px; opacity: 0.85; padding: 6px 10px; border: 1px solid #4445; border-radius: 999px; }
    .card { border: 1px solid #4444; border-radius: 12px; padding: 12px; }
    .grid { display: grid; gap: 12px; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); }
    .item { display: grid; gap: 8px; }
    .thumb { height: 140px; display: grid; place-items: center; border-radius: 10px; overflow: hidden; border: 1px solid #4444; background: #1111; }
    .thumb img { max-width: 100%; max-height: 100%; image-rendering: auto; }
    .meta { font-size: 12px; opacity: 0.85; word-break: break-all; }
    input[type="text"] { padding: 9px 10px; border-radius: 10px; border: 1px solid #4446; background: transparent; min-width: 240px; }
    input[type="file"] { max-width: 360px; }
    button { padding: 9px 10px; border-radius: 10px; border: 1px solid #4446; background: transparent; cursor: pointer; }
    button.primary { border: 0; background: #16a34a; color: white; font-weight: 600; }
    button.danger { border: 0; background: #ef4444; color: white; font-weight: 600; }
    .top { display: grid; gap: 12px; margin-bottom: 16px; }
    .url { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace; font-size: 12px; opacity: 0.9; word-break: break-all; }
    .msg { font-size: 12px; opacity: 0.9; }
    .edit { display: grid; gap: 8px; }
    .edit input[type="text"] { min-width: 0; width: 100%; }
  </style>
</head>
<body>
  <header>
    <div class="row">
      <h1>emoji-server 管理</h1>
      <div class="pill">外部 URL 前缀：<span class="url" id="baseUrl"></span></div>
    </div>
    <form method="post" action="/admin/logout">
      <button type="submit">退出</button>
    </form>
  </header>

  <div class="top">
    <div class="card">
      <div class="row" style="justify-content: space-between;">
        <div style="display:grid; gap:6px;">
          <div><b>上传</b></div>
          <div class="msg">支持多选上传与覆盖同名文件。文件名支持中文，但不能包含 &lt; &gt; : " / \ | ? *，也不能以点或空格结尾。</div>
        </div>
      </div>
      <div class="row" style="margin-top:10px;">
        <input id="file" type="file" multiple />
        <input id="name" type="text" placeholder="可选：自定义文件名（仅单文件，如 过生日.gif）" />
        <button class="primary" id="upload">上传</button>
        <button id="refresh">刷新</button>
        <span class="msg" id="status"></span>
      </div>
    </div>

    <div class="grid" id="grid"></div>
  </div>

  <script>
    const CFG = {
      publicKey: {{.PublicKeyJSON}},
      baseURL: {{.BaseURLJSON}},
    };

    function $(id) { return document.getElementById(id); }

    function guessBaseURL() {
      if (CFG.baseURL && CFG.baseURL.trim()) return CFG.baseURL.trim().replace(/\/+$/, '');
      return location.origin;
    }

    function publicURL(name) {
      const base = guessBaseURL();
      return base + '/e/' + CFG.publicKey + '/' + name;
    }

    function publicURLFetch(name) {
      const base = guessBaseURL();
      return base + '/e/' + encodeURIComponent(CFG.publicKey) + '/' + encodeURIComponent(name);
    }

    async function api(path, opts) {
      const res = await fetch(path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts || {}));
      const text = await res.text();
      let data = null;
      try { data = JSON.parse(text); } catch (_) {}
      if (!res.ok) {
        const msg = (data && data.error) ? data.error : ('HTTP ' + res.status);
        throw new Error(msg);
      }
      return data;
    }

    function setStatus(msg) {
      $('status').textContent = msg || '';
    }

    async function loadFiles() {
      setStatus('加载中...');
      const data = await api('/api/admin/files', { method: 'GET', headers: {} });
      const grid = $('grid');
      grid.innerHTML = '';
      const files = (data && data.files) ? data.files : [];
      if (!files.length) {
        const empty = document.createElement('div');
        empty.className = 'card';
        empty.textContent = '暂无资源。';
        grid.appendChild(empty);
      }
      for (const f of files) {
        const card = document.createElement('div');
        card.className = 'card item';

        const thumb = document.createElement('div');
        thumb.className = 'thumb';
        const img = document.createElement('img');
        img.loading = 'lazy';
        img.alt = f.name;
        img.src = publicURLFetch(f.name);
        thumb.appendChild(img);

        const meta = document.createElement('div');
        meta.className = 'meta';
        const url = publicURL(f.name);

        const title = document.createElement('div');
        const b = document.createElement('b');
        b.textContent = f.name;
        title.appendChild(b);

        const urlDiv = document.createElement('div');
        urlDiv.className = 'url';
        urlDiv.textContent = url;

        const edit = document.createElement('div');
        edit.className = 'edit';
        const newName = document.createElement('input');
        newName.type = 'text';
        newName.placeholder = '新文件名（如 过生日.gif）';
        newName.value = f.name;
        const renameRow = document.createElement('div');
        renameRow.className = 'row';
        const renameBtn = document.createElement('button');
        renameBtn.textContent = '重命名';
        renameBtn.onclick = async () => {
          const v = (newName.value || '').trim();
          if (!v) { setStatus('请输入新文件名'); return; }
          if (v === f.name) { setStatus('文件名未变化'); return; }
          try {
            await api('/api/admin/rename', { method: 'POST', body: JSON.stringify({ old_name: f.name, new_name: v }) });
            await loadFiles();
            setStatus('已重命名：' + f.name + ' -> ' + v);
          } catch (e) { setStatus('重命名失败：' + e.message); }
        };
        renameRow.appendChild(renameBtn);
        edit.appendChild(newName);
        edit.appendChild(renameRow);

        meta.appendChild(title);
        meta.appendChild(urlDiv);
        meta.appendChild(edit);

        const row = document.createElement('div');
        row.className = 'row';
        const copy = document.createElement('button');
        copy.textContent = '复制 URL';
        copy.onclick = async () => {
          try { await navigator.clipboard.writeText(url); setStatus('已复制：' + f.name); }
          catch (_) { setStatus('复制失败（浏览器权限限制）'); }
        };
        const del = document.createElement('button');
        del.className = 'danger';
        del.textContent = '删除';
        del.onclick = async () => {
          if (!confirm('确认删除：' + f.name + ' ?')) return;
          try {
            await api('/api/admin/delete', { method: 'POST', body: JSON.stringify({ name: f.name }) });
            await loadFiles();
            setStatus('已删除：' + f.name);
          } catch (e) { setStatus('删除失败：' + e.message); }
        };
        row.appendChild(copy);
        row.appendChild(del);

        card.appendChild(thumb);
        card.appendChild(meta);
        card.appendChild(row);
        grid.appendChild(card);
      }
      setStatus('完成。共 ' + files.length + ' 个文件');
    }

    function escapeHTML(s) {
      return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
    }

    async function upload() {
      const files = Array.from($('file').files || []);
      if (!files.length) { setStatus('请选择文件'); return; }
      const name = $('name').value.trim();
      if (files.length > 1 && name) { setStatus('多选上传不支持自定义文件名'); return; }

      const form = new FormData();
      for (const f of files) form.append('file', f);
      if (name) form.append('name', name);

      setStatus('上传中...');
      const res = await fetch('/api/admin/upload', { method: 'POST', body: form });
      const text = await res.text();
      let data = null;
      try { data = JSON.parse(text); } catch (_) {}
      if (!res.ok) {
        const msg = (data && data.error) ? data.error : ('HTTP ' + res.status);
        setStatus('上传失败：' + msg);
        return;
      }
      $('file').value = '';
      $('name').value = '';
      await loadFiles();
      const names = (data && data.names) ? data.names : (data && data.name ? [data.name] : []);
      setStatus('已上传：' + names.join(', '));
    }

    $('file').addEventListener('change', () => {
      const n = ($('file').files || []).length;
      if (n > 1) {
        $('name').value = '';
        $('name').disabled = true;
        $('name').placeholder = '多选时不支持自定义文件名';
      } else {
        $('name').disabled = false;
        $('name').placeholder = '可选：自定义文件名（仅单文件，如 过生日.gif）';
      }
    });
    $('upload').addEventListener('click', (e) => { e.preventDefault(); upload(); });
    $('refresh').addEventListener('click', (e) => { e.preventDefault(); loadFiles(); });
    $('baseUrl').textContent = publicURL('<filename>');
    loadFiles().catch(e => setStatus('加载失败：' + e.message));
  </script>
</body>
</html>`))

func renderAdmin(w http.ResponseWriter, cfg Config) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	publicKeyJSON, _ := json.Marshal(cfg.PublicKey)
	baseURLJSON, _ := json.Marshal(cfg.BaseURL)
	_ = adminTpl.Execute(w, struct {
		PublicKeyJSON template.JS
		BaseURLJSON   template.JS
	}{
		PublicKeyJSON: template.JS(publicKeyJSON),
		BaseURLJSON:   template.JS(baseURLJSON),
	})
}
