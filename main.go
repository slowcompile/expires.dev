package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxUpload  = 64 << 20
	maxAge     = 24 * time.Hour
	uploadsDir = "uploads"
	metaDir    = "meta"
)

//go:embed adjectives.txt
var adjectivesFile string

//go:embed index.html
var indexPage string

var adjectives = strings.Split(strings.TrimSpace(strings.ReplaceAll(adjectivesFile, "\r\n", "\n")), "\n")
var extRe = regexp.MustCompile(`^\.[a-z0-9]+$`)

type uploadMeta struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	OrigName   string    `json:"orig_name"`
	UploadedAt time.Time `json:"uploaded_at"`
}

func writeMeta(name string, m uploadMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(metaDir, name+".json"), b, 0o600)
}

func main() {
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		log.Fatal(err)
	}

	// deletes all files older than maxAge every minute
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cleanupOldFiles(uploadsDir, maxAge)
			cleanupOldFiles(metaDir, maxAge)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("POST /", handleUpload)
	mux.HandleFunc("GET /{name}", handleFile)

	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", mux))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexPage)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "invalid multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if len(ext) > 10 || !extRe.MatchString(ext) {
		ext = ""
	}

	// pick a name, retry on collision
	var dst *os.File
	var name, dstPath string
	for attempt := 0; attempt < 10; attempt++ {
		name = pickRandomAdjectives(2) + ext
		dstPath = filepath.Join(uploadsDir, name)
		dst, err = os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if dst == nil {
		http.Error(w, "could not allocate filename, try again", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	n, err := io.Copy(dst, file)
	if err != nil {
		_ = os.Remove(dstPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	meta := uploadMeta{
		Name:       name,
		Size:       n,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		OrigName:   header.Filename,
		UploadedAt: time.Now().UTC(),
	}
	if err := writeMeta(name, meta); err != nil {
		_ = os.Remove(dstPath)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s/%s", scheme, r.Host, name)
	fmt.Fprintf(w, "%s\n", url)
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// dont allow path traversal or hidden files
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(uploadsDir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// don't let browsers execute html / js
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	http.ServeFile(w, r, path)
}

func pickRandomAdjectives(n int) string {
	picked := make([]string, n)
	perm := rand.Perm(len(adjectives))
	for i := 0; i < n; i++ {
		picked[i] = adjectives[perm[i]]
	}
	return strings.Join(picked, "")
}

func cleanupOldFiles(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("cleanup: %v", err)
		return
	}

	cutoff := time.Now().Add(-maxAge)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			path := filepath.Join(dir, entry.Name())
			if err := os.Remove(path); err != nil {
				log.Printf("cleanup: failed to remove %s: %v", path, err)
			} else {
				log.Printf("cleanup: removed %s", path)
			}
		}
	}
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	// fallback
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
