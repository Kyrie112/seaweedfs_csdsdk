package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// M3:版本保留与延迟回收(MVCC 式)。
//
// 目标:计算进行中时对同一个卷做整理(compaction),在途请求仍然读到它开始时
// 的**那一份数据**,而不是失败或被换掉的数据。
//
// 实现思路(对既有整理路径改动最小):
//
//  1. 为当前数据文件建立一个**硬链接**,命名为 `<base>.dat.g<代次>`;
//     硬链接与 `.dat` 指向同一个 inode,不复制数据;
//  2. 计算请求的区间描述符使用**这个版本路径**,而不是固定的 `.dat`;
//  3. 整理提交时,`os.RemoveAll(.dat)` 只删除目录项——由于版本硬链接仍指向该
//     inode,旧数据依然可读,新数据由新的 `.dat` 提供;
//  4. 版本硬链接在有请求使用它时被标记"最近使用",只有**超过保留期且不是当前
//     代次**时才被删除(延迟回收)。
//
// 这样:老任务读老 inode,新请求读新 `.dat`,互不影响;回收则交给 TTL 兜底。

const (
	// versionFileSuffix 追加在 ".dat" 之后(如 3.dat.g7),因此**不会**以 .idx/.vif
	// 结尾,不会被 DiskLocation 的卷扫描逻辑当成新卷加载。
	versionFileSuffix = ".g"

	// DefaultVersionRetention 是版本被回收前的默认保留期。
	// 取值依据:远大于一次计算请求的耗时(本地实测为毫秒级),
	// 从而"老任务还在跑"时不会发生回收;真正需要更长保留的场景应显式调大。
	DefaultVersionRetention = 10 * time.Minute

	// DefaultMaxRetainedVersions 限制单卷同时保留的历史版本数,防止空间失控。
	DefaultMaxRetainedVersions = 4
)

// versionState 保存代次与每个历史版本的最近使用时间。
type versionState struct {
	mu         sync.Mutex
	generation uint64
	lastUsed   map[uint64]time.Time
}

// versionFilePath 返回某个代次对应的数据文件路径。
func (v *Volume) versionFilePath(generation uint64) string {
	return v.FileName(".dat") + versionFileSuffix + strconv.FormatUint(generation, 10)
}

// CurrentDataGeneration 返回当前数据文件代次。
func (v *Volume) CurrentDataGeneration() uint64 {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	return v.versions.generation
}

// bumpDataGeneration 在整理提交完成后调用(调用点已持有写锁)。
func (v *Volume) bumpDataGeneration() uint64 {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	v.versions.generation++
	now := time.Now()
	// 下一代次刚成为当前代次,给它一个"最近使用"起点,避免被立即回收。
	v.versions.lastUsed[v.versions.generation] = now
	return v.versions.generation
}

// initVersionState 在卷加载时调用:从目录中已有的版本文件推断当前代次,
// 避免重启后重复使用同一个代次编号。
func (v *Volume) initVersionState() {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	if v.versions.lastUsed == nil {
		v.versions.lastUsed = make(map[uint64]time.Time)
	}
	now := time.Now()
	highest := uint64(0)
	for _, generation := range v.existingVersionGenerations() {
		if generation > highest {
			highest = generation
		}
		// A restarted volume server has no memory of which versions were in use,
		// so give every version found on disk a fresh retention window instead of
		// trusting its file mtime (which may be hours old and would allow an
		// immediate, unsafe reclaim).
		v.versions.lastUsed[generation] = now
	}
	v.versions.generation = highest
	v.versions.lastUsed[highest] = now
}

// existingVersionGenerations 列出磁盘上已存在的版本文件代次。
func (v *Volume) existingVersionGenerations() []uint64 {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return nil
	}
	base := filepath.Base(v.FileName(".dat")) + versionFileSuffix
	var generations []uint64
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		generation, err := strconv.ParseUint(strings.TrimPrefix(name, base), 10, 64)
		if err != nil {
			continue
		}
		generations = append(generations, generation)
	}
	sort.Slice(generations, func(i, j int) bool { return generations[i] < generations[j] })
	return generations
}

// ensureVersionLink 保证"当前代次"的硬链接存在,并返回其路径。
//
// 调用者必须持有 dataFileAccessLock 的读锁:这样它与整理(持写锁)互斥,
// 链接一定指向整理发生**之前**的那个 inode。
func (v *Volume) ensureVersionLink() (string, error) {
	generation := v.CurrentDataGeneration()
	linkPath := v.versionFilePath(generation)

	if _, err := os.Stat(linkPath); err == nil {
		return linkPath, nil
	}
	livePath := v.FileName(".dat")
	if _, err := os.Stat(livePath); err != nil {
		return "", fmt.Errorf("version link: live data file unavailable: %w", err)
	}
	if err := os.Link(livePath, linkPath); err != nil {
		if os.IsExist(err) {
			return linkPath, nil
		}
		return "", fmt.Errorf("version link %s: %w", linkPath, err)
	}
	glog.V(2).Infof("volume %d: retained data version %d at %s", v.Id, generation, linkPath)
	return linkPath, nil
}

// noteVersionUse 记录"有请求正在使用某个代次",用于延迟回收。
func (v *Volume) noteVersionUse(generation uint64) {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	if v.versions.lastUsed == nil {
		v.versions.lastUsed = make(map[uint64]time.Time)
	}
	v.versions.lastUsed[generation] = time.Now()
}

// versionsToReclaim 计算可以回收的版本文件(纯函数,便于测试)。
//
// 规则:永远不回收当前代次;只回收"最近使用时间早于 now-retention"的版本;
// 并且最多保留 maxVersions 个历史版本(即使它们已经过期)。
func (v *Volume) versionsToReclaim(now time.Time, retention time.Duration, maxVersions int) []string {
	v.versions.mu.Lock()
	current := v.versions.generation
	lastUsed := make(map[uint64]time.Time, len(v.versions.lastUsed))
	for generation, at := range v.versions.lastUsed {
		lastUsed[generation] = at
	}
	v.versions.mu.Unlock()

	generations := v.existingVersionGenerations()
	// 从新到旧保留 maxVersions 个历史版本。
	kept := make(map[uint64]struct{})
	historical := 0
	for i := len(generations) - 1; i >= 0; i-- {
		if generations[i] == current {
			continue
		}
		historical++
		if historical <= maxVersions {
			kept[generations[i]] = struct{}{}
		}
	}

	var expired []string
	for _, generation := range generations {
		if generation == current {
			continue
		}
		if _, keep := kept[generation]; keep {
			continue
		}
		usedAt, recorded := lastUsed[generation]
		if !recorded {
			// 没有使用记录(例如卷在保留期内重启):用文件自身的修改时间兜底,
			// 这样仍然满足"至少保留 retention 这么久"的语义。
			info, err := os.Stat(v.versionFilePath(generation))
			if err != nil {
				continue
			}
			usedAt = info.ModTime()
		}
		if now.Sub(usedAt) >= retention {
			expired = append(expired, v.versionFilePath(generation))
		}
	}
	return expired
}

// gcExpiredVersions 删除可以回收的版本文件(GC 的实际动作)。
func (v *Volume) gcExpiredVersions(now time.Time, retention time.Duration, maxVersions int) []string {
	var removed []string
	for _, path := range v.versionsToReclaim(now, retention, maxVersions) {
		if err := os.Remove(path); err != nil {
			glog.Warningf("volume %d: cannot reclaim data version %s: %v", v.Id, path, err)
			continue
		}
		glog.V(1).Infof("volume %d: reclaimed data version %s", v.Id, path)
		removed = append(removed, path)
	}
	return removed
}

// GarbageCollectDataVersions 按默认策略回收过期版本(供整理流程与测试调用)。
func (v *Volume) GarbageCollectDataVersions() []string {
	return v.gcExpiredVersions(time.Now(), DefaultVersionRetention, DefaultMaxRetainedVersions)
}

// GarbageCollectDataVersionsWithRetention 允许调用方指定保留期,便于运维在磁盘
// 压力下提前回收,也便于验证工具用很短的保留期驱动 GC。
func (v *Volume) GarbageCollectDataVersionsWithRetention(retention time.Duration, maxVersions int) []string {
	if retention < 0 {
		retention = DefaultVersionRetention
	}
	if maxVersions <= 0 {
		maxVersions = DefaultMaxRetainedVersions
	}
	return v.gcExpiredVersions(time.Now(), retention, maxVersions)
}

// DataVersionInfo 描述一个被保留的数据版本,用于运维观测。
type DataVersionInfo struct {
	Path       string `json:"path"`
	Generation uint64 `json:"generation"`
	Current    bool   `json:"current"`
	LastUsed   string `json:"last_used,omitempty"`
}

// DataVersionInfos 列出当前被保留的所有数据版本。
func (v *Volume) DataVersionInfos() []DataVersionInfo {
	v.versions.mu.Lock()
	current := v.versions.generation
	lastUsed := make(map[uint64]time.Time, len(v.versions.lastUsed))
	for generation, at := range v.versions.lastUsed {
		lastUsed[generation] = at
	}
	v.versions.mu.Unlock()

	infos := make([]DataVersionInfo, 0)
	for _, generation := range v.existingVersionGenerations() {
		info := DataVersionInfo{
			Path:       v.versionFilePath(generation),
			Generation: generation,
			Current:    generation == current,
		}
		if at, ok := lastUsed[generation]; ok {
			info.LastUsed = at.UTC().Format(time.RFC3339)
		}
		infos = append(infos, info)
	}
	return infos
}
