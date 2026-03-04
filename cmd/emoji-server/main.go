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
	Category    string    `json:"category"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
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

func (s *FileStore) ListCategories(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := sanitizePathSegment(name); err != nil {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (s *FileStore) ListFiles(ctx context.Context, category string) ([]FileInfo, error) {
	catSafe, err := sanitizePathSegment(category)
	if err != nil {
		return nil, err
	}
	catDir := filepath.Join(s.dir, catSafe)
	entries, err := os.ReadDir(catDir)
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
		if _, err := sanitizePathSegment(name); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		rel := catSafe + "/" + name
		out = append(out, FileInfo{
			Category: catSafe,
			Name:     name,
			Path:     rel,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *FileStore) CreateCategory(category string) (string, error) {
	catSafe, err := sanitizePathSegment(category)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(s.dir, catSafe), 0o755); err != nil {
		return "", err
	}
	return catSafe, nil
}

func (s *FileStore) RenameCategory(oldCategory, newCategory string) (string, error) {
	oldSafe, err := sanitizePathSegment(oldCategory)
	if err != nil {
		return "", err
	}
	newSafe, err := sanitizePathSegment(newCategory)
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
		tmp, err := os.CreateTemp(s.dir, ".cat-*")
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
	return "", errors.New("重命名文件夹失败")
}

func (s *FileStore) DeleteCategory(category string) error {
	catSafe, err := sanitizePathSegment(category)
	if err != nil {
		return err
	}
	catDir := filepath.Join(s.dir, catSafe)
	entries, err := os.ReadDir(catDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("文件夹非空")
	}
	return os.Remove(catDir)
}

func (s *FileStore) List(ctx context.Context) ([]FileInfo, error) {
	cats, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, 128)
	for _, cat := range cats {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !cat.IsDir() {
			continue
		}
		category := cat.Name()
		if category == "" || strings.HasPrefix(category, ".") {
			continue
		}
		if _, err := sanitizePathSegment(category); err != nil {
			continue
		}
		catDir := filepath.Join(s.dir, category)
		files, err := os.ReadDir(catDir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if name == "" || strings.HasPrefix(name, ".") {
				continue
			}
			if _, err := sanitizePathSegment(name); err != nil {
				continue
			}
			info, err := f.Info()
			if err != nil {
				return nil, err
			}
			rel := category + "/" + name
			out = append(out, FileInfo{
				Category: category,
				Name:     name,
				Path:     rel,
				Size:     info.Size(),
				ModTime:  info.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category == out[j].Category {
			return out[i].Name < out[j].Name
		}
		return out[i].Category < out[j].Category
	})
	return out, nil
}

func (s *FileStore) Open(category, name string) (string, *os.File, error) {
	catSafe, err := sanitizePathSegment(category)
	if err != nil {
		return "", nil, err
	}
	nameSafe, err := sanitizePathSegment(name)
	if err != nil {
		return "", nil, err
	}
	full := filepath.Join(s.dir, catSafe, nameSafe)
	f, err := os.Open(full)
	if err != nil {
		return "", nil, err
	}
	return full, f, nil
}

func (s *FileStore) Save(category, name string, r io.Reader) (string, error) {
	catSafe, err := sanitizePathSegment(category)
	if err != nil {
		return "", err
	}
	nameSafe, err := sanitizePathSegment(name)
	if err != nil {
		return "", err
	}
	catDir := filepath.Join(s.dir, catSafe)
	if err := os.MkdirAll(catDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(catDir, nameSafe)

	tmp, err := os.CreateTemp(catDir, ".upload-*")
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
	return catSafe + "/" + nameSafe, nil
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

func (s *FileStore) Delete(category, name string) error {
	catSafe, err := sanitizePathSegment(category)
	if err != nil {
		return err
	}
	nameSafe, err := sanitizePathSegment(name)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.dir, catSafe, nameSafe))
}

func (s *FileStore) Rename(oldCategory, oldName, newCategory, newName string) (string, error) {
	oldCatSafe, err := sanitizePathSegment(oldCategory)
	if err != nil {
		return "", err
	}
	oldNameSafe, err := sanitizePathSegment(oldName)
	if err != nil {
		return "", err
	}
	newCatSafe, err := sanitizePathSegment(newCategory)
	if err != nil {
		return "", err
	}
	newNameSafe, err := sanitizePathSegment(newName)
	if err != nil {
		return "", err
	}
	if oldCatSafe == newCatSafe && oldNameSafe == newNameSafe {
		return newCatSafe + "/" + newNameSafe, nil
	}

	oldPath := filepath.Join(s.dir, oldCatSafe, oldNameSafe)
	newDir := filepath.Join(s.dir, newCatSafe)
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return "", err
	}
	newPath := filepath.Join(newDir, newNameSafe)

	if _, err := os.Stat(oldPath); err != nil {
		return "", err
	}
	if _, err := os.Stat(newPath); err == nil {
		return "", os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.Rename(oldPath, newPath); err == nil {
		return newCatSafe + "/" + newNameSafe, nil
	}

	if oldCatSafe == newCatSafe && strings.EqualFold(oldNameSafe, newNameSafe) {
		tmp, err := os.CreateTemp(newDir, ".rename-*")
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
		return newCatSafe + "/" + newNameSafe, nil
	}

	return "", errors.New("重命名失败")
}

func (s *FileStore) Copy(fromCategory, fromName, toCategory, toName string) (string, error) {
	fromCatSafe, err := sanitizePathSegment(fromCategory)
	if err != nil {
		return "", err
	}
	fromNameSafe, err := sanitizePathSegment(fromName)
	if err != nil {
		return "", err
	}
	toCatSafe, err := sanitizePathSegment(toCategory)
	if err != nil {
		return "", err
	}
	toNameSafe, err := sanitizePathSegment(toName)
	if err != nil {
		return "", err
	}

	srcPath := filepath.Join(s.dir, fromCatSafe, fromNameSafe)
	dstDir := filepath.Join(s.dir, toCatSafe)
	dstPath := filepath.Join(dstDir, toNameSafe)

	if _, err := os.Stat(srcPath); err != nil {
		return "", err
	}
	if _, err := os.Stat(dstPath); err == nil {
		return "", os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(dstDir, ".copy-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return "", err
	}
	return toCatSafe + "/" + toNameSafe, nil
}

func sanitizePathSegment(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
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

	migrateRootFiles(store, "未分类")

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
	mux.HandleFunc("/api/admin/categories", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		cats, err := store.ListCategories(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
	}))

	mux.HandleFunc("/api/admin/category/create", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
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
		name, err := store.CreateCategory(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
	}))

	mux.HandleFunc("/api/admin/category/rename", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
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
		oldSafe, err := sanitizePathSegment(req.OldName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		newSafe, err := sanitizePathSegment(req.NewName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		out, err := store.RenameCategory(oldSafe, newSafe)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件夹不存在"})
				return
			}
			if errors.Is(err, os.ErrExist) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "目标文件夹已存在"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": out})
	}))

	mux.HandleFunc("/api/admin/category/delete", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
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
		name, err := sanitizePathSegment(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := store.DeleteCategory(name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件夹不存在"})
				return
			}
			if err.Error() == "文件夹非空" {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "文件夹非空"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/admin/files", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		category := strings.TrimSpace(r.URL.Query().Get("category"))
		if category == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 category"})
			return
		}
		files, err := store.ListFiles(r.Context(), category)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件夹不存在"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
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
		category := strings.TrimSpace(r.FormValue("category"))
		if category == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 category（分类）"})
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
			out, err := store.Save(category, name, f)
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

	mux.HandleFunc("/api/admin/file/rename", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Category string `json:"category"`
			OldName  string `json:"old_name"`
			NewName  string `json:"new_name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		catSafe, err := sanitizePathSegment(req.Category)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		oldSafe, err := sanitizePathSegment(req.OldName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		newSafe, err := sanitizePathSegment(req.NewName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		oldRel := catSafe + "/" + oldSafe
		newRel := catSafe + "/" + newSafe
		if oldRel == newRel {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": newRel})
			return
		}
		out, err := store.Rename(catSafe, oldSafe, catSafe, newSafe)
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": out})
	}))

	mux.HandleFunc("/api/admin/file/move", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			FromCategory string `json:"from_category"`
			Name         string `json:"name"`
			ToCategory   string `json:"to_category"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		fromCat, err := sanitizePathSegment(req.FromCategory)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		toCat, err := sanitizePathSegment(req.ToCategory)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		name, err := sanitizePathSegment(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if fromCat == toCat {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": fromCat + "/" + name})
			return
		}
		out, err := store.Rename(fromCat, name, toCat, name)
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": out})
	}))

	mux.HandleFunc("/api/admin/file/copy", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			FromCategory string `json:"from_category"`
			Name         string `json:"name"`
			ToCategory   string `json:"to_category"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		fromCat, err := sanitizePathSegment(req.FromCategory)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		toCat, err := sanitizePathSegment(req.ToCategory)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		name, err := sanitizePathSegment(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if fromCat == toCat {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "同文件夹内复制需要改名，当前不支持"})
			return
		}
		out, err := store.Copy(fromCat, name, toCat, name)
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": out})
	}))

	mux.HandleFunc("/api/admin/file/delete", requireAdmin(sessions, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Category string `json:"category"`
			Name     string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON 格式错误"})
			return
		}
		catSafe, err := sanitizePathSegment(req.Category)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		nameSafe, err := sanitizePathSegment(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := store.Delete(catSafe, nameSafe); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/pw=") {
			servePublic(w, r, cfg.PublicKey, store)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           withLogging(handler),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("监听失败：%v", err)
	}
	log.Printf("emoji-server listening on %s", ln.Addr().String())
	log.Printf("admin: http://%s/admin", hostForLog(ln.Addr()))
	log.Printf("public: http://%s/pw=%s/<分类>/<文件名>", hostForLog(ln.Addr()), cfg.PublicKey)
	log.Fatal(srv.Serve(ln))
}

func migrateRootFiles(store *FileStore, defaultCategory string) {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return
	}
	rootFiles := make([]string, 0, 16)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if _, err := sanitizePathSegment(name); err != nil {
			continue
		}
		rootFiles = append(rootFiles, name)
	}
	if len(rootFiles) == 0 {
		return
	}
	catSafe, err := store.CreateCategory(defaultCategory)
	if err != nil {
		return
	}
	for _, name := range rootFiles {
		src := filepath.Join(store.dir, name)
		dst := filepath.Join(store.dir, catSafe, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			log.Printf("migrate root file failed: %s -> %s: %v", src, dst, err)
			continue
		}
	}
}

func servePublic(w http.ResponseWriter, r *http.Request, publicKey string, store *FileStore) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	i := strings.IndexByte(p, '/')
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	keySeg := p[:i]
	rest := p[i+1:]
	if keySeg == "" || rest == "" {
		http.NotFound(w, r)
		return
	}

	if v, err := url.PathUnescape(keySeg); err == nil {
		keySeg = v
	}
	if !strings.HasPrefix(keySeg, "pw=") {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimPrefix(keySeg, "pw=")
	if key != publicKey {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	category := parts[0]
	name := parts[1]
	if v, err := url.PathUnescape(category); err == nil {
		category = v
	}
	if v, err := url.PathUnescape(name); err == nil {
		name = v
	}

	full, f, err := store.Open(category, name)
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
    *, *::before, *::after { box-sizing: border-box; }
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
    *, *::before, *::after { box-sizing: border-box; }
    body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; padding: 18px; }
    header { display: flex; gap: 12px; align-items: center; justify-content: space-between; flex-wrap: wrap; margin-bottom: 16px; }
    h1 { font-size: 18px; margin: 0; }
    .row { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
    .pill { font-size: 12px; opacity: 0.85; padding: 6px 10px; border: 1px solid #4445; border-radius: 999px; }
    .card { border: 1px solid #4444; border-radius: 12px; padding: 12px; }
    .grid { display: grid; gap: 12px; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); }
    .item { display: grid; gap: 8px; }
    .thumb { height: 140px; display: grid; place-items: center; border-radius: 10px; overflow: hidden; border: 1px solid #4444; background: #1111; }
    .thumb img { max-width: 100%; max-height: 100%; image-rendering: auto; cursor: zoom-in; }
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
    textarea.names { width: 100%; min-height: 64px; resize: vertical; padding: 10px 12px; border-radius: 10px; border: 1px solid #4446; background: transparent; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace; font-size: 12px; }
    select { padding: 9px 10px; border-radius: 10px; border: 1px solid #4446; background: transparent; min-width: 240px; }
    .modal { position: fixed; inset: 0; padding: 24px; background: rgba(0,0,0,0.62); display: none; align-items: center; justify-content: center; z-index: 1000; }
    .modal.on { display: flex; }
    .modal .panel { width: min(560px, 96vw); }
    .viewer { position: fixed; inset: 0; padding: 24px; background: rgba(0,0,0,0.72); display: none; align-items: center; justify-content: center; z-index: 999; }
    .viewer.on { display: flex; }
    .viewer .box { position: relative; max-width: 96vw; max-height: 92vh; display: grid; gap: 10px; }
    .viewer .imgwrap { position: relative; display: grid; place-items: center; }
    .viewer img { max-width: 96vw; max-height: 80vh; border-radius: 12px; border: 1px solid #fff2; background: #000; }
    .viewer .cap { font-size: 12px; opacity: 0.92; display: flex; justify-content: space-between; gap: 10px; align-items: center; }
    .viewer .cap .name { word-break: break-all; }
    .viewer .cap button { border: 1px solid #fff3; background: #0006; color: inherit; }
    .viewer .nav { position: absolute; top: 50%; transform: translateY(-50%); padding: 10px 12px; border-radius: 999px; border: 1px solid #fff3; background: #0006; color: inherit; cursor: pointer; }
    .viewer .nav.left { left: 10px; }
    .viewer .nav.right { right: 10px; }
  </style>
</head>
<body>
  <header>
    <div class="row">
      <h1>emoji-server 管理</h1>
      <button id="backBtn" type="button" style="display:none;">返回文件夹</button>
      <div class="pill">当前位置：<span class="url" id="crumb">文件夹</span></div>
      <div class="pill">外部 URL 前缀：<span class="url" id="baseUrl"></span></div>
      <span class="msg" id="status"></span>
    </div>
    <form method="post" action="/admin/logout">
      <button type="submit">退出</button>
    </form>
  </header>

  <div class="top">
    <div class="card" id="folderCard">
      <div class="row" style="justify-content: space-between;">
        <div style="display:grid; gap:6px;">
          <div><b>文件夹</b></div>
          <div class="msg">先进入文件夹，再在里面上传表情包。支持创建/重命名/删除文件夹（删除要求文件夹为空）。</div>
        </div>
      </div>
      <div class="row" style="margin-top:10px;">
        <input id="newCat" type="text" placeholder="新建文件夹（分类名，如 小美表情包）" />
        <button class="primary" id="createCat" type="button">创建</button>
        <button id="refresh" type="button">刷新</button>
      </div>
    </div>

    <div class="card" id="uploadCard" style="display:none;">
      <div class="row" style="justify-content: space-between;">
        <div style="display:grid; gap:6px;">
          <div><b>上传到：<span id="currentCat"></span></b></div>
          <div class="msg">支持多选上传与覆盖同名文件。文件名支持中文，但不能包含 &lt; &gt; : " / \ | ? *，也不能以点或空格结尾。</div>
        </div>
      </div>
      <div class="row" style="margin-top:10px;">
        <input id="file" type="file" multiple />
        <input id="name" type="text" placeholder="可选：自定义文件名（仅单文件，如 开心.gif）" />
        <button class="primary" id="upload" type="button">上传</button>
        <button id="refreshFiles" type="button">刷新</button>
      </div>
    </div>

    <div class="card" id="namesCard" style="display:none;">
      <div class="row" style="justify-content: space-between;">
        <div style="display:grid; gap:6px;">
          <div><b>名称列表（当前文件夹）</b></div>
          <div class="msg">一键复制当前文件夹下所有文件名（用 <code>|</code> 分隔）。</div>
        </div>
        <button id="copyNames" type="button">复制</button>
      </div>
      <div style="margin-top:10px;">
        <textarea id="namesBlock" class="names" readonly></textarea>
      </div>
    </div>

    <div class="grid" id="grid"></div>
  </div>

  <div id="viewer" class="viewer" aria-hidden="true">
    <div class="box">
      <div class="cap">
        <div class="name" id="viewerName"></div>
        <button id="viewerClose" type="button">关闭</button>
      </div>
      <div class="imgwrap">
        <button class="nav left" id="viewerPrev" type="button">←</button>
        <img id="viewerImg" alt="">
        <button class="nav right" id="viewerNext" type="button">→</button>
      </div>
    </div>
  </div>

  <div id="transfer" class="modal" aria-hidden="true">
    <div class="card panel">
      <div class="row" style="justify-content: space-between;">
        <div><b id="transferTitle">移动/复制</b></div>
        <button id="transferClose" type="button">关闭</button>
      </div>
      <div class="msg" style="margin-top:8px;">文件：<span class="url" id="transferFile"></span></div>
      <div class="row" style="margin-top:10px;">
        <select id="transferTarget"></select>
        <button class="primary" id="transferOk" type="button">确定</button>
      </div>
      <div class="msg" style="margin-top:8px;">提示：不会覆盖同名文件。</div>
    </div>
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

    function publicURL(category, name) {
      const base = guessBaseURL();
      return base + '/pw=' + CFG.publicKey + '/' + category + '/' + name;
    }

    function publicURLFetch(category, name) {
      const base = guessBaseURL();
      return base + '/pw=' + encodeURIComponent(CFG.publicKey) + '/' + encodeURIComponent(category) + '/' + encodeURIComponent(name);
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

    function show(el, on) {
      if (!el) return;
      el.style.display = on ? '' : 'none';
    }

    function currentCategory() {
      const h = (location.hash || '').trim();
      if (!h.startsWith('#cat=')) return '';
      const raw = h.slice(5);
      try { return decodeURIComponent(raw).trim(); } catch (_) { return raw.trim(); }
    }

    function setCategory(cat) {
      const v = (cat || '').trim();
      if (!v) { location.hash = ''; return; }
      location.hash = 'cat=' + encodeURIComponent(v);
    }

    function clearCategory() {
      location.hash = '';
    }

    let CATEGORIES = [];
    let FILES = [];
    let viewerIndex = -1;
    let transferState = null;

    function renderNamesBlock() {
      const names = (FILES || []).map(x => x.name || '').filter(Boolean);
      const text = names.join('|');
      $('namesBlock').value = text;
    }

    async function copyText(text) {
      try {
        await navigator.clipboard.writeText(text);
        return true;
      } catch (_) {}
      try {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.left = '-9999px';
        ta.style.top = '0';
        document.body.appendChild(ta);
        ta.focus();
        ta.select();
        const ok = document.execCommand('copy');
        document.body.removeChild(ta);
        return ok;
      } catch (_) {
        return false;
      }
    }

    function viewerIsOpen() {
      return $('viewer').classList.contains('on');
    }

    function viewerRender() {
      if (viewerIndex < 0 || viewerIndex >= FILES.length) return;
      const f = FILES[viewerIndex];
      $('viewerName').textContent = (f.category || '') + '/' + (f.name || '');
      const img = $('viewerImg');
      img.alt = (f.category || '') + '/' + (f.name || '');
      img.src = publicURLFetch(f.category, f.name);
    }

    function viewerOpen(i) {
      if (!FILES.length) return;
      viewerIndex = (i % FILES.length + FILES.length) % FILES.length;
      viewerRender();
      $('viewer').classList.add('on');
      $('viewer').setAttribute('aria-hidden', 'false');
    }

    function viewerClose() {
      $('viewer').classList.remove('on');
      $('viewer').setAttribute('aria-hidden', 'true');
      $('viewerImg').src = '';
      viewerIndex = -1;
    }

    function viewerStep(delta) {
      if (!FILES.length) return;
      if (viewerIndex < 0) viewerIndex = 0;
      viewerIndex = (viewerIndex + delta + FILES.length) % FILES.length;
      viewerRender();
    }

    async function ensureCategories() {
      if (CATEGORIES && CATEGORIES.length) return CATEGORIES;
      const data = await api('/api/admin/categories', { method: 'GET', headers: {} });
      CATEGORIES = (data && data.categories) ? data.categories : [];
      return CATEGORIES;
    }

    async function openTransfer(action, f) {
      const fromCat = currentCategory();
      if (!fromCat) { setStatus('请先进入一个文件夹'); return; }
      const cats = await ensureCategories();
      const targets = cats.filter(x => x !== fromCat);
      if (!targets.length) { setStatus('没有可选目标文件夹'); return; }

      transferState = { action, fromCat, name: f.name };
      $('transferTitle').textContent = action === 'move' ? '移动' : '复制';
      $('transferFile').textContent = f.name;

      const sel = $('transferTarget');
      sel.innerHTML = '';
      for (const c of targets) {
        const opt = document.createElement('option');
        opt.value = c;
        opt.textContent = c;
        sel.appendChild(opt);
      }

      $('transfer').classList.add('on');
      $('transfer').setAttribute('aria-hidden', 'false');
    }

    function closeTransfer() {
      $('transfer').classList.remove('on');
      $('transfer').setAttribute('aria-hidden', 'true');
      transferState = null;
    }

    function applyView() {
      const cat = currentCategory();
      const inFolder = !!cat;
      show($('folderCard'), !inFolder);
      show($('uploadCard'), inFolder);
      show($('namesCard'), inFolder);
      show($('backBtn'), inFolder);
      $('crumb').textContent = inFolder ? cat : '文件夹';
      $('currentCat').textContent = inFolder ? cat : '';
      if (!inFolder) {
        if ($('transfer').classList.contains('on')) closeTransfer();
        if (viewerIsOpen()) viewerClose();
        FILES = [];
        viewerIndex = -1;
        if ($('namesBlock')) $('namesBlock').value = '';
      }
    }

    async function loadCategories() {
      setStatus('加载中...');
      applyView();
      const grid = $('grid');
      grid.innerHTML = '';
      const data = await api('/api/admin/categories', { method: 'GET', headers: {} });
      CATEGORIES = (data && data.categories) ? data.categories : [];
      if (!CATEGORIES.length) {
        const empty = document.createElement('div');
        empty.className = 'card';
        empty.textContent = '暂无文件夹。';
        grid.appendChild(empty);
      }
      for (const cat of CATEGORIES) {
        const card = document.createElement('div');
        card.className = 'card item';

        const title = document.createElement('div');
        const b = document.createElement('b');
        b.textContent = cat;
        title.appendChild(b);

        const edit = document.createElement('div');
        edit.className = 'edit';
        const newName = document.createElement('input');
        newName.type = 'text';
        newName.placeholder = '新文件夹名';
        newName.value = cat;
        const row1 = document.createElement('div');
        row1.className = 'row';
        const openBtn = document.createElement('button');
        openBtn.className = 'primary';
        openBtn.textContent = '进入';
        openBtn.onclick = () => setCategory(cat);
        const renameBtn = document.createElement('button');
        renameBtn.textContent = '重命名';
        renameBtn.onclick = async () => {
          const v = (newName.value || '').trim();
          if (!v) { setStatus('请输入新文件夹名'); return; }
          if (v === cat) { setStatus('文件夹名未变化'); return; }
          try {
            await api('/api/admin/category/rename', { method: 'POST', body: JSON.stringify({ old_name: cat, new_name: v }) });
            setStatus('已重命名文件夹：' + cat + ' -> ' + v);
            if (currentCategory() === cat) setCategory(v);
            await loadCategories();
          } catch (e) { setStatus('重命名失败：' + e.message); }
        };
        const delBtn = document.createElement('button');
        delBtn.className = 'danger';
        delBtn.textContent = '删除';
        delBtn.onclick = async () => {
          if (!confirm('确认删除文件夹（需为空）：' + cat + ' ?')) return;
          try {
            await api('/api/admin/category/delete', { method: 'POST', body: JSON.stringify({ name: cat }) });
            setStatus('已删除文件夹：' + cat);
            await loadCategories();
          } catch (e) { setStatus('删除失败：' + e.message); }
        };
        row1.appendChild(openBtn);
        row1.appendChild(renameBtn);
        row1.appendChild(delBtn);
        edit.appendChild(newName);
        edit.appendChild(row1);

        card.appendChild(title);
        card.appendChild(edit);
        grid.appendChild(card);
      }
      setStatus('完成。共 ' + CATEGORIES.length + ' 个文件夹');
    }

    async function loadFiles(cat) {
      setStatus('加载中...');
      applyView();
      const grid = $('grid');
      grid.innerHTML = '';
      const data = await api('/api/admin/files?category=' + encodeURIComponent(cat), { method: 'GET', headers: {} });
      const files = (data && data.files) ? data.files : [];
      const openPath = (viewerIndex >= 0 && viewerIndex < FILES.length) ? FILES[viewerIndex].path : '';
      FILES = files;
      renderNamesBlock();
      if (openPath) {
        const ni = FILES.findIndex(x => x.path === openPath);
        if (ni >= 0) {
          viewerIndex = ni;
          if (viewerIsOpen()) viewerRender();
        } else if (viewerIsOpen()) {
          viewerClose();
        }
      }
      if (!files.length) {
        const empty = document.createElement('div');
        empty.className = 'card';
        empty.textContent = '暂无表情包。';
        grid.appendChild(empty);
      }
      for (let idx = 0; idx < files.length; idx++) {
        const f = files[idx];
        const card = document.createElement('div');
        card.className = 'card item';

        const thumb = document.createElement('div');
        thumb.className = 'thumb';
        const img = document.createElement('img');
        img.loading = 'lazy';
        img.alt = f.name;
        img.src = publicURLFetch(cat, f.name);
        img.onclick = () => viewerOpen(idx);
        thumb.appendChild(img);

        const meta = document.createElement('div');
        meta.className = 'meta';
        const url = publicURL(cat, f.name);
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
        newName.placeholder = '新文件名';
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
            await api('/api/admin/file/rename', { method: 'POST', body: JSON.stringify({ category: cat, old_name: f.name, new_name: v }) });
            await loadFiles(cat);
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
            await api('/api/admin/file/delete', { method: 'POST', body: JSON.stringify({ category: cat, name: f.name }) });
            await loadFiles(cat);
            setStatus('已删除：' + f.name);
          } catch (e) { setStatus('删除失败：' + e.message); }
        };
        const moveBtn = document.createElement('button');
        moveBtn.textContent = '移动';
        moveBtn.onclick = async () => {
          try { await openTransfer('move', f); } catch (e) { setStatus('加载文件夹失败：' + e.message); }
        };
        const copyBtn = document.createElement('button');
        copyBtn.textContent = '复制';
        copyBtn.onclick = async () => {
          try { await openTransfer('copy', f); } catch (e) { setStatus('加载文件夹失败：' + e.message); }
        };
        row.appendChild(copy);
        row.appendChild(moveBtn);
        row.appendChild(copyBtn);
        row.appendChild(del);

        card.appendChild(thumb);
        card.appendChild(meta);
        card.appendChild(row);
        grid.appendChild(card);
      }
      setStatus('完成。共 ' + files.length + ' 个文件');
    }

    async function upload() {
      const cat = currentCategory();
      if (!cat) { setStatus('请先进入一个文件夹'); return; }
      const files = Array.from($('file').files || []);
      if (!files.length) { setStatus('请选择文件'); return; }
      const name = $('name').value.trim();
      if (files.length > 1 && name) { setStatus('多选上传不支持自定义文件名'); return; }

      const form = new FormData();
      form.append('category', cat);
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
      await loadFiles(cat);
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
    $('refresh').addEventListener('click', async (e) => { e.preventDefault(); await loadCategories(); });
    $('refreshFiles').addEventListener('click', async (e) => { e.preventDefault(); const cat = currentCategory(); if (cat) await loadFiles(cat); });
    $('baseUrl').textContent = guessBaseURL() + '/pw=' + CFG.publicKey + '/<分类>/<文件名>';
    $('backBtn').addEventListener('click', (e) => { e.preventDefault(); clearCategory(); });
    $('createCat').addEventListener('click', async (e) => {
      e.preventDefault();
      const name = ($('newCat').value || '').trim();
      if (!name) { setStatus('请输入文件夹名'); return; }
      try {
        await api('/api/admin/category/create', { method: 'POST', body: JSON.stringify({ name }) });
        $('newCat').value = '';
        await loadCategories();
        setStatus('已创建文件夹：' + name);
      } catch (err) {
        setStatus('创建失败：' + err.message);
      }
    });
    $('copyNames').addEventListener('click', async (e) => {
      e.preventDefault();
      const text = $('namesBlock').value || '';
      if (!text) { setStatus('暂无文件名可复制'); return; }
      const ok = await copyText(text);
      setStatus(ok ? '已复制文件名列表' : '复制失败（浏览器权限限制）');
    });
    $('viewer').addEventListener('click', (e) => { if (e.target === $('viewer')) viewerClose(); });
    $('viewerClose').addEventListener('click', (e) => { e.preventDefault(); viewerClose(); });
    $('viewerPrev').addEventListener('click', (e) => { e.preventDefault(); viewerStep(-1); });
    $('viewerNext').addEventListener('click', (e) => { e.preventDefault(); viewerStep(1); });
    $('transfer').addEventListener('click', (e) => { if (e.target === $('transfer')) closeTransfer(); });
    $('transferClose').addEventListener('click', (e) => { e.preventDefault(); closeTransfer(); });
    $('transferOk').addEventListener('click', async (e) => {
      e.preventDefault();
      if (!transferState) return;
      const toCat = ($('transferTarget').value || '').trim();
      if (!toCat) { setStatus('请选择目标文件夹'); return; }
      const payload = { from_category: transferState.fromCat, name: transferState.name, to_category: toCat };
      try {
        if (transferState.action === 'move') {
          await api('/api/admin/file/move', { method: 'POST', body: JSON.stringify(payload) });
          closeTransfer();
          await loadFiles(transferState.fromCat);
          setStatus('已移动：' + transferState.name + ' -> ' + toCat);
        } else {
          await api('/api/admin/file/copy', { method: 'POST', body: JSON.stringify(payload) });
          closeTransfer();
          setStatus('已复制：' + transferState.name + ' -> ' + toCat);
        }
      } catch (err) {
        setStatus((transferState.action === 'move' ? '移动失败：' : '复制失败：') + err.message);
      }
    });
    document.addEventListener('keydown', (e) => {
      if ($('transfer').classList.contains('on') && e.key === 'Escape') {
        closeTransfer();
        return;
      }
      if (!viewerIsOpen()) return;
      if (e.key === 'Escape') viewerClose();
      if (e.key === 'ArrowLeft') viewerStep(-1);
      if (e.key === 'ArrowRight') viewerStep(1);
    });
    window.addEventListener('hashchange', () => {
      const cat = currentCategory();
      if (cat) loadFiles(cat).catch(e => setStatus('加载失败：' + e.message));
      else loadCategories().catch(e => setStatus('加载失败：' + e.message));
    });
    applyView();
    const cat = currentCategory();
    if (cat) loadFiles(cat).catch(e => setStatus('加载失败：' + e.message));
    else loadCategories().catch(e => setStatus('加载失败：' + e.message));
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
