package store

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// AntiRefusalPath 是本书尾部防拒写原文，相对输出根目录。
const AntiRefusalPath = "meta/anti_refusal.md"

const maxAntiRefusalBytes = 1024 * 1024

// AntiRefusalStatus 是 inspect 与 loader 共用的文件状态。
type AntiRefusalStatus string

const (
	AntiRefusalMissing AntiRefusalStatus = "missing"
	AntiRefusalEmpty   AntiRefusalStatus = "empty"
	AntiRefusalFile    AntiRefusalStatus = "file"
	AntiRefusalInvalid AntiRefusalStatus = "invalid"
)

// AntiRefusalLoad 是一次读取结果。Text 非空才应注入。
type AntiRefusalLoad struct {
	Text    string
	Status  AntiRefusalStatus
	Path    string
	SHA256  string
	Warning string
}

// AntiRefusalStore 读取本书 meta/anti_refusal.md。缺文件或空文件不注入。
type AntiRefusalStore struct {
	io *IO

	mu          sync.Mutex
	cachedMod   time.Time
	cachedSize  int64
	cachedLoad  AntiRefusalLoad
	cacheFilled bool
}

func NewAntiRefusalStore(io *IO) *AntiRefusalStore { return &AntiRefusalStore{io: io} }

// LoadText 返回应追加的 reminder 原文；没有可注入内容时返回空串。
func (s *AntiRefusalStore) LoadText() string {
	if s == nil {
		return ""
	}
	return s.Load().Text
}

// Load 读取并校验文件。按 mtime+size 缓存，改文件后下一轮 Generate 生效。
func (s *AntiRefusalStore) Load() AntiRefusalLoad {
	if s == nil || s.io == nil {
		return AntiRefusalLoad{Path: AntiRefusalPath, Status: AntiRefusalMissing}
	}

	s.io.mu.RLock()
	abs := s.io.path(AntiRefusalPath)
	fi, err := os.Lstat(abs)
	if err != nil {
		s.io.mu.RUnlock()
		out := AntiRefusalLoad{Path: abs, Status: AntiRefusalMissing}
		if !os.IsNotExist(err) {
			out.Status = AntiRefusalInvalid
			out.Warning = err.Error()
			slog.Warn("读取防拒写文件失败", "module", "store", "path", abs, "err", err)
		}
		s.storeCache(time.Time{}, 0, out)
		return out
	}
	mod, size := fi.ModTime(), fi.Size()
	if hit, cached := s.cacheHit(mod, size); hit {
		s.io.mu.RUnlock()
		return cached
	}
	data, readErr := os.ReadFile(abs)
	s.io.mu.RUnlock()

	out := AntiRefusalLoad{Path: abs}
	if readErr != nil {
		out.Status = AntiRefusalInvalid
		out.Warning = readErr.Error()
		slog.Warn("读取防拒写文件失败", "module", "store", "path", abs, "err", readErr)
		s.storeCache(mod, size, out)
		return out
	}
	if int64(len(data)) > maxAntiRefusalBytes {
		out.Status = AntiRefusalInvalid
		out.Warning = "file exceeds 1 MiB"
		slog.Warn("忽略过大的防拒写文件", "module", "store", "path", abs, "bytes", len(data))
		s.storeCache(mod, size, out)
		return out
	}
	if !utf8.Valid(data) {
		out.Status = AntiRefusalInvalid
		out.Warning = "not valid UTF-8"
		slog.Warn("忽略无效防拒写文件", "module", "store", "path", abs)
		s.storeCache(mod, size, out)
		return out
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		out.Status = AntiRefusalEmpty
		s.storeCache(mod, size, out)
		return out
	}
	sum := sha256.Sum256(data)
	out.Status = AntiRefusalFile
	out.Text = text
	out.SHA256 = hex.EncodeToString(sum[:])
	s.storeCache(mod, size, out)
	return out
}

func (s *AntiRefusalStore) cacheHit(mod time.Time, size int64) (bool, AntiRefusalLoad) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cacheFilled && s.cachedMod.Equal(mod) && s.cachedSize == size {
		return true, s.cachedLoad
	}
	return false, AntiRefusalLoad{}
}

func (s *AntiRefusalStore) storeCache(mod time.Time, size int64, load AntiRefusalLoad) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cachedMod = mod
	s.cachedSize = size
	s.cachedLoad = load
	s.cacheFilled = true
}
