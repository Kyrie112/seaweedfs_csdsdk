package weed_server

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// M1(可检测)实现:把"这次计算针对文件哪个版本"变成可传递、可校验的令牌。
//
// 设计目标是让最危险的两类静默错误变成显式失败:
//   - 后台整理(vacuum/compaction)在计算期间移动了数据;
//   - needle 被删除/过期后,其物理区间被新的写入复用。
//
// 令牌通过已有的算子参数下推通道(params)传递,因此不需要新增协议字段;
// 老调用方不带令牌时按 L0(尽力读)处理,行为与改造前一致。

// 下推通道的保留键名。新增算子参数时不要使用这些名字。
const (
	computeParamSnapshot     = "snap"    // 快照摘要(12 位十六进制)
	computeParamChunkIndex   = "chunk"   // 本块在清单中的序号
	computeParamChunkSize    = "csize"   // filer 期望的该块数据长度
	computeParamNeedleCookie = "ncookie" // needle cookie(由 volume 填充)
	computeParamNeedleId     = "nid"     // needle id(由 volume 填充)
)

// 一致性等级(见分布式一致性 PLAN 4.1)。
const (
	// consistencyBestEffort:不校验版本,失败则降级。缺省行为与改造前一致。
	consistencyBestEffort = "L0"
	// consistencyReadCommitted:每个块的数据必须是某个已提交版本的完整数据。
	// 这是默认等级——以最小代价消除"读到别的 needle 或被移动的字节"。
	consistencyReadCommitted = "L1"
	// consistencySnapshot:所有块必须来自同一文件版本,否则重试或显式失败。
	consistencySnapshot = "L2"
)

// 一致性相关的响应头。
const (
	consistencyHeaderLevel    = "X-Compute-Consistency-Level"
	consistencyHeaderSnapshot = "X-Compute-Snapshot"
	consistencyHeaderStale    = "X-Compute-Stale-Chunks"
	consistencyHeaderRetry    = "X-Compute-Retry-Reason"

	// 设备侧判定区间失效的响应头(由 volume 从 agent 透传)。
	csdHeaderStaleRange = "X-CSD-Stale-Range"
)

// 设备侧返回的错误标识,volume 用它来区分"节点故障"与"数据已变"。
const staleRangeErrorCode = "stale_range"

// errStaleRange 表示"物理区间已不再属于目标对象"。
//
// 这类失败**绝不能**降级到脚本路径:脚本路径读的是完整 needle,若数据已被
// 改写,它会安静地算出另一份数据的结果——正是 M1 要消除的静默错误。
var errStaleRange = errors.New(staleRangeErrorCode)

// computeSnapshot 描述"这次计算针对文件的哪个版本"。
//
// entryVersion 取自 filer 条目的修改时间(秒 + 纳秒)。当前 filer store 不保证
// 提供单调递增的 revision,因此用 mtime 作为条目版本;它足以区分"计算开始后
// 文件被改写"的情形,但不构成严格的偏序保证——这一点在文档中明确记录。
type computeSnapshot struct {
	filePath     string
	entryVersion string
	digest       string
}

// newComputeSnapshot 在展开并校验完块清单之后调用一次。
//
// 要求调用方传入**同一份**已校验的清单,这样令牌才能代表"本次请求的版本视角"。
func newComputeSnapshot(filePath string, entry *filer.Entry, chunks []*filer_pb.FileChunk) computeSnapshot {
	version := ""
	if entry != nil && !entry.Mtime.IsZero() {
		version = strconv.FormatInt(entry.Mtime.UnixNano(), 10)
	}
	return computeSnapshot{
		filePath:     filePath,
		entryVersion: version,
		digest:       digestChunks(chunks),
	}
}

// digestChunks 对规范化后的块清单求摘要。
//
// 摘要只反映"块集合与布局",因此只要清单不变,摘要就不变;一旦块被替换、
// 长度变化或顺序改变,摘要即变化。
func digestChunks(chunks []*filer_pb.FileChunk) string {
	hasher := fnv.New64a()
	for index, chunk := range chunks {
		fmt.Fprintf(hasher, "%d:%d:%d:%s;",
			index, chunk.GetOffset(), chunk.GetSize(), chunk.GetFileIdString())
	}
	return fmt.Sprintf("%012x", hasher.Sum64()&0xFFFFFFFFFFFF)
}

// String 返回便于日志与响应头使用的紧凑表示。
func (s computeSnapshot) String() string {
	return fmt.Sprintf("%s@%s/%s", s.filePath, s.entryVersion, s.digest)
}

// tokenParams 返回下推该块令牌所需的参数。
//
// 注意:这些键名是保留的,算子自身的参数不得与之冲突。
func (s computeSnapshot) tokenParams(index int, payloadBytes int64) map[string]string {
	return map[string]string{
		computeParamSnapshot:   s.digest,
		computeParamChunkIndex: strconv.Itoa(index),
		computeParamChunkSize:  strconv.FormatInt(payloadBytes, 10),
	}
}

// consistencyLevelFromQuery 解析请求指定的一致性等级,缺省为 L1。
//
// 该参数在 M1 只用于标注;M2 才会让 L2 触发重试与失败。
func consistencyLevelFromQuery(raw string) string {
	switch strings.ToLower(raw) {
	case "l0":
		return consistencyBestEffort
	case "l2":
		return consistencySnapshot
	default:
		return consistencyReadCommitted
	}
}

// verifyChunkTokens 在聚合前校验各块是否来自同一快照且序号齐全。
//
// 返回值:实际达到的等级、观测到的不一致块数、以及是否通过。
// L0/L1 下不一致只做标注(L1 的强校验已在 volume 与设备侧完成);
// L2 下不一致直接判定为失败。
func verifyChunkTokens(level string, expected computeSnapshot, echoed []string, indexes []string, chunkCount int) (string, int, bool) {
	stale := 0
	seen := make(map[string]struct{}, len(echoed))
	for _, digest := range echoed {
		if digest == "" {
			stale++ // 老 agent 不回传:视为"未标注快照"
			continue
		}
		if digest != expected.digest {
			stale++
		}
		seen[digest] = struct{}{}
	}
	if len(indexes) != chunkCount {
		stale += chunkCount - len(indexes)
	}
	if level == consistencySnapshot && stale > 0 {
		return level, stale, false
	}
	// L0/L1:结果仍然可用(每块的强校验已在设备侧完成),但要如实标注。
	if stale > 0 {
		return consistencyBestEffort, stale, true
	}
	return level, 0, true
}
