package handler

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

const maxFileSize = 1 << 20 // 1 MB

// FileHandler serves read-only browsing of files under a root directory.
type FileHandler struct {
	rootDir string
}

// NewFileHandler returns a handler that serves files rooted at rootDir.
func NewFileHandler(rootDir string) *FileHandler {
	return &FileHandler{rootDir: rootDir}
}

type fileEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// List responds with the directory entries at the requested path.
func (h *FileHandler) List(c *gin.Context) {
	rel := c.Query("path")
	resolved, err := h.resolve(rel)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "directory not found"})
		return
	}
	files := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			Name:    entry.Name(),
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	c.JSON(http.StatusOK, files)
}

// Read responds with the contents of the file at the requested path.
func (h *FileHandler) Read(c *gin.Context) {
	rel := c.Param("path")
	// gin wildcard includes leading slash.
	rel = strings.TrimPrefix(rel, "/")
	resolved, err := h.resolve(rel)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is a directory"})
		return
	}
	if info.Size() > maxFileSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file too large (max 1MB)"})
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path":    rel,
		"content": string(data),
	})
}

// resolve validates and resolves a relative path under rootDir.
func (h *FileHandler) resolve(rel string) (string, error) {
	clean := filepath.Clean(rel)
	resolved := filepath.Join(h.rootDir, clean)
	// Ensure the resolved path is still under rootDir.
	absRoot, err := filepath.Abs(h.rootDir)
	if err != nil {
		return "", err
	}
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absResolved, absRoot) {
		return "", os.ErrPermission
	}
	return absResolved, nil
}
