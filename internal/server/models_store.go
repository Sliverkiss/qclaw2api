// models_store.go /v1/models 文件化读取：启动加载 + mtime 重载 + staticModels 回退。
package server

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// modelsStore 模型文件读取器。
type modelsStore struct {
	path  string
	mu    sync.RWMutex
	data  []map[string]any // OpenAI 格式模型列表
	mtime time.Time
}

// newModelsStore 启动时加载；失败回退 staticModels。
func newModelsStore(path string) *modelsStore {
	s := &modelsStore{path: path}
	if path != "" {
		s.reload()
	}
	if s.data == nil {
		s.data = staticModels
	}
	return s
}

// list 返回模型列表：stat mtime 变更则重读；文件缺失/解析失败回退 staticModels。
func (s *modelsStore) list() []map[string]any {
	if s.path != "" {
		if fi, err := os.Stat(s.path); err == nil {
			s.mu.RLock()
			stale := !fi.ModTime().Equal(s.mtime)
			s.mu.RUnlock()
			if stale {
				s.reload()
			}
		} else {
			// 文件被删除 → 回退静态表
			s.mu.Lock()
			if s.data != nil {
				s.data = staticModels
				s.mtime = time.Time{}
			}
			s.mu.Unlock()
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// reload 读取 models.json（OpenAI /v1/models 形状）并缓存；失败回退 staticModels。
func (s *modelsStore) reload() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		log.Printf("models_store: read %s: %v (fallback to static)", s.path, err)
		return
	}
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("models_store: parse %s: %v (fallback to static)", s.path, err)
		return
	}
	if len(doc.Data) == 0 {
		log.Printf("models_store: %s has empty data (fallback to static)", s.path)
		return
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.data = doc.Data
	s.mtime = fi.ModTime()
	s.mu.Unlock()
}
