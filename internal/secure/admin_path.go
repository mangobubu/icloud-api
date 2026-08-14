package secure

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const adminPathSuffix = "/admin"

// LoadOrCreateAdminPath returns the stable, installation-specific management
// prefix. The file contains the complete path with a trailing slash so it can
// be copied directly from the keys volume by an operator.
func LoadOrCreateAdminPath(filename, configured string) (string, bool, error) {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return "", false, errors.New("管理路径文件不能为空")
	}
	configured = strings.TrimSpace(configured)
	if configured != "" {
		path, err := normalizeAdminPath(configured)
		if err != nil {
			return "", false, err
		}
		created, err := persistAdminPath(filename, path)
		return path, created, err
	}

	data, err := os.ReadFile(filename)
	if err == nil {
		path, validateErr := normalizeAdminPath(string(data))
		if validateErr != nil {
			return "", false, fmt.Errorf("读取管理路径文件: %w", validateErr)
		}
		return path, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("读取管理路径文件: %w", err)
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false, fmt.Errorf("生成管理路径: %w", err)
	}
	path := "/" + hex.EncodeToString(random[:]) + adminPathSuffix
	created, err := persistAdminPath(filename, path)
	if err == nil && !created {
		data, readErr := os.ReadFile(filename)
		if readErr != nil {
			return "", false, fmt.Errorf("读取并发生成的管理路径: %w", readErr)
		}
		path, readErr = normalizeAdminPath(string(data))
		if readErr != nil {
			return "", false, fmt.Errorf("读取并发生成的管理路径: %w", readErr)
		}
	}
	return path, created, err
}

func persistAdminPath(filename, path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return false, fmt.Errorf("创建管理路径目录: %w", err)
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := os.ReadFile(filename)
		if readErr != nil {
			return false, fmt.Errorf("读取已有管理路径: %w", readErr)
		}
		existing, validateErr := normalizeAdminPath(string(data))
		if validateErr != nil {
			return false, validateErr
		}
		if existing != path {
			return false, errors.New("配置的管理路径与持久化路径不一致")
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("创建管理路径文件: %w", err)
	}
	writeErr := error(nil)
	if _, err := file.WriteString(path + "/\n"); err != nil {
		writeErr = err
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(filename)
		return false, fmt.Errorf("写入管理路径文件: %w", writeErr)
	}
	return true, nil
}

func normalizeAdminPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "/")
	if len(value) != 1+32+len(adminPathSuffix) || value[0] != '/' ||
		value[33:] != adminPathSuffix {
		return "", errors.New("管理路径必须为 /<32位小写十六进制>/admin/")
	}
	for _, char := range value[1:33] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", errors.New("管理路径必须为 /<32位小写十六进制>/admin/")
		}
	}
	return value, nil
}

// NormalizeAdminPath validates an externally supplied management prefix and
// returns its canonical no-trailing-slash form.
func NormalizeAdminPath(value string) (string, error) {
	return normalizeAdminPath(value)
}
