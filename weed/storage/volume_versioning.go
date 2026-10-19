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

// M3:版本保留与延迟回收(MVCC / epoch 风格)。
//
// 目标:计算进行中时对同一个卷做整理(compaction),在途请求仍然读到它开始时
// 的那一份数据,而不是失败或被换掉的数据。
//
// 设计(借鉴 PostgreSQL 的快照可见性、RCU/epoch 回收与 etcd 租约):
//
//  1. 为当前数据文件建立硬链接 `<base>.dat.g<代次>`;硬链接与 `.dat` 共享
//     inode,不复制数据。整理提交时 `os.RemoveAll(.dat)` 只删除目录项,
//     旧数据仍可读,新数据由新的 `.dat` 提供;
//  2. 计算描述符指向**版本链接**,而不是固定的 `.dat`;
//  3. 读取前 `AcquireVersionLease` 钉住代次,读完 `ReleaseVersionLease`;
//     进程崩溃时租约按 TTL 自动过期(崩溃兜底);
//  4. 回收器采用**低水位**:只有"没有任何未过期租约"的旧代次才可回收,
//     再叠加保留期与每卷历史版本数上限;
//  5. 代次持久化到 `<base>.dat.gen`,重启后不复用旧代次。
//
// 与最初"最近使用 + TTL"的版本相比,租约让回收条件从"猜测没人用"变成
// "确认没人用";TTL 仅作为崩溃/异常路径的兜底。

const (
	// versionFileSuffix 追加在 ".dat" 之后(如 3.dat.g7),因此**不会**以 .idx/.vif
	// 结尾,不会被 DiskLocation 的卷扫描逻辑当成新卷加载。
	versionFileSuffix = ".g"

	// DefaultVersionRetention 是版本在没有活跃租约时,仍需等待的宽限期。
	DefaultVersionRetention = 10 * time.Minute

	// DefaultMaxRetainedVersions 限制单卷同时保留的历史版本数,防止空间失控。
	DefaultMaxRetainedVersions = 4

	// DefaultVersionLeaseTTL 是租约的崩溃兜底时间。正常路径读完后会立即释放,
	// 该值只用于"进程崩溃/请求异常"时自动解钉。
	DefaultVersionLeaseTTL = 30 * time.Second
)

// versionLease 是一次读取对某个数据版本的显式租约。
type versionLease struct {
	id         uint64
	generation uint64
	expiresAt  time.Time
}

type versionState struct {
	mu          sync.Mutex
	generation  uint64
	lastUsed    map[uint64]time.Time
	leases      map[uint64]*versionLease
	nextLeaseID uint64
}

// versionFilePath 返回某个代次对应的数据文件路径。
func (v *Volume) versionFilePath(generation uint64) string {
	return v.FileName(".dat") + versionFileSuffix + strconv.FormatUint(generation, 10)
}

// generationFilePath 保存代次的持久化记录。
func (v *Volume) generationFilePath() string {
	return v.FileName(".dat") + ".gen"
}

// CurrentDataGeneration 返回当前数据文件代次。
func (v *Volume) CurrentDataGeneration() uint64 {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	return v.versions.generation
}

// persistGeneration 原子写入代次(临时文件 + fsync + rename)。
func (v *Volume) persistGeneration(generation uint64) {
	path := v.generationFilePath()
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.Warningf("volume %d: cannot persist data generation: %v", v.Id, err)
		return
	}
	if _, err := file.WriteString(strconv.FormatUint(generation, 10)); err != nil {
		file.Close()
		os.Remove(tmp)
		glog.Warningf("volume %d: cannot write data generation: %v", v.Id, err)
		return
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		glog.Warningf("volume %d: cannot fsync data generation: %v", v.Id, err)
		return
	}
	file.Close()
	if err := os.Rename(tmp, path); err != nil {
		glog.Warningf("volume %d: cannot publish data generation: %v", v.Id, err)
	}
}

func (v *Volume) readPersistedGeneration() (uint64, bool) {
	raw, err := os.ReadFile(v.generationFilePath())
	if err != nil {
		return 0, false
	}
	generation, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return generation, true
}

// bumpDataGeneration 在整理提交完成后调用(调用点已持有写锁)。
func (v *Volume) bumpDataGeneration() uint64 {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	v.versions.generation++
	generation := v.versions.generation
	if v.versions.lastUsed == nil {
		v.versions.lastUsed = make(map[uint64]time.Time)
	}
	v.versions.lastUsed[generation] = time.Now()
	v.persistGeneration(generation)
	return generation
}

// initVersionState 在卷加载时恢复代次,并给现存版本一个保守的保留窗口。
func (v *Volume) initVersionState() {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	if v.versions.lastUsed == nil {
		v.versions.lastUsed = make(map[uint64]time.Time)
	}
	if v.versions.leases == nil {
		v.versions.leases = make(map[uint64]*versionLease)
	}

	now := time.Now()
	highest := uint64(0)
	if persisted, ok := v.readPersistedGeneration(); ok {
		highest = persisted
	}
	for _, generation := range v.existingVersionGenerations() {
		if generation > highest {
			highest = generation
		}
		// 重启后没有在途租约信息,给每个现存版本一个完整保留窗口,
		// 而不是信任可能很旧的文件 mtime。
		v.versions.lastUsed[generation] = now
	}
	v.versions.generation = highest
	v.versions.lastUsed[highest] = now
	v.persistGeneration(highest)
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

// noteVersionUse 记录某个代次最近一次被使用(租约之外的辅助信号)。
func (v *Volume) noteVersionUse(generation uint64) {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	if v.versions.lastUsed == nil {
		v.versions.lastUsed = make(map[uint64]time.Time)
	}
	v.versions.lastUsed[generation] = time.Now()
}

// AcquireVersionLease 钉住一个代次,返回租约 ID。
//
// 调用方在**真正读取数据之前**获取租约、读完立即 Release;TTL 是崩溃兜底。
func (v *Volume) AcquireVersionLease(generation uint64, ttl time.Duration) uint64 {
	if ttl <= 0 {
		ttl = DefaultVersionLeaseTTL
	}
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	if v.versions.leases == nil {
		v.versions.leases = make(map[uint64]*versionLease)
	}
	v.versions.nextLeaseID++
	id := v.versions.nextLeaseID
	v.versions.leases[id] = &versionLease{
		id:         id,
		generation: generation,
		expiresAt:  time.Now().Add(ttl),
	}
	return id
}

// ReleaseVersionLease 释放租约(幂等)。
func (v *Volume) ReleaseVersionLease(id uint64) {
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	delete(v.versions.leases, id)
}

// RenewVersionLease 续租(长读场景)。
func (v *Volume) RenewVersionLease(id uint64, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = DefaultVersionLeaseTTL
	}
	v.versions.mu.Lock()
	defer v.versions.mu.Unlock()
	lease, ok := v.versions.leases[id]
	if !ok {
		return false
	}
	lease.expiresAt = time.Now().Add(ttl)
	return true
}

// pinnedGenerationsLocked 返回仍被未过期租约钉住的代次 -> 活跃租约数。
// 调用者必须持有 versions.mu。
func (v *Volume) pinnedGenerationsLocked(now time.Time) map[uint64]int {
	pinned := make(map[uint64]int)
	for id, lease := range v.versions.leases {
		if now.After(lease.expiresAt) {
			delete(v.versions.leases, id) // 过期租约:崩溃兜底
			continue
		}
		pinned[lease.generation]++
	}
	return pinned
}

// versionsToReclaim 计算可以回收的版本文件(纯函数,便于测试)。
//
// 规则:永远不回收当前代次;不回收被未过期租约钉住的代次;只回收超过保留期的
// 版本;并且最多保留 maxVersions 个历史版本。
func (v *Volume) versionsToReclaim(now time.Time, retention time.Duration, maxVersions int) []string {
	v.versions.mu.Lock()
	current := v.versions.generation
	lastUsed := make(map[uint64]time.Time, len(v.versions.lastUsed))
	for generation, at := range v.versions.lastUsed {
		lastUsed[generation] = at
	}
	pinned := v.pinnedGenerationsLocked(now)
	v.versions.mu.Unlock()

	generations := v.existingVersionGenerations()
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
		// 低水位:有任何未过期租约钉住该版本时,绝不回收。
		if pinned[generation] > 0 {
			continue
		}
		usedAt, recorded := lastUsed[generation]
		if !recorded {
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

// gcExpiredVersions 删除可以回收的版本文件。
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

// GarbageCollectDataVersions 按默认策略回收过期版本。
func (v *Volume) GarbageCollectDataVersions() []string {
	return v.gcExpiredVersions(time.Now(), DefaultVersionRetention, DefaultMaxRetainedVersions)
}

// GarbageCollectDataVersionsWithRetention 允许调用方指定保留期。
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
	Leases     int    `json:"leases"`
}

// DataVersionInfos 列出当前被保留的所有数据版本。
func (v *Volume) DataVersionInfos() []DataVersionInfo {
	v.versions.mu.Lock()
	current := v.versions.generation
	lastUsed := make(map[uint64]time.Time, len(v.versions.lastUsed))
	for generation, at := range v.versions.lastUsed {
		lastUsed[generation] = at
	}
	pinned := v.pinnedGenerationsLocked(time.Now())
	v.versions.mu.Unlock()

	infos := make([]DataVersionInfo, 0)
	for _, generation := range v.existingVersionGenerations() {
		info := DataVersionInfo{
			Path:       v.versionFilePath(generation),
			Generation: generation,
			Current:    generation == current,
			Leases:     pinned[generation],
		}
		if at, ok := lastUsed[generation]; ok {
			info.LastUsed = at.UTC().Format(time.RFC3339)
		}
		infos = append(infos, info)
	}
	return infos
}
