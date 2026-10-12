package weed_server

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"

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
	consistencyHeaderError    = "X-Compute-Error-Class"
	consistencyHeaderDataVer  = "X-Compute-Data-Version"

	// 设备侧判定区间失效的响应头(由 volume 从 agent 透传)。
	csdHeaderStaleRange = "X-CSD-Stale-Range"
	// csdHeaderDataVersion 是副本回报的"我实际服务了哪个数据版本"。
	csdHeaderDataVersion = "X-CSD-Data-Version"
	// csdHeaderErrorClass 让错误分类跨进程传递,避免靠字符串匹配。
	csdHeaderErrorClass = "X-CSD-Error-Class"

	// computeQueryParamOnStale 控制 stale_range 的处理方式:
	// retry(默认,重新解析清单后重试一次)或 fail(直接失败)。
	computeQueryParamOnStale = "on_stale"
)

// hostPreference 记录"本次请求已经用过哪些副本主机"。
//
// 作用:同一次计算请求的多个数据块尽量落在同一组副本上,减少因副本复制进度
// 不同而反复失败重试(M2/T2.1)。它是软偏好——只有当该主机的负载仍在
// "最小负载 + 一个数据块"的余量内时才优先,避免把负载均衡彻底压垮。
type hostPreference struct {
	mu    sync.Mutex
	hosts map[string]struct{}
}

func newHostPreference() *hostPreference {
	return &hostPreference{hosts: make(map[string]struct{})}
}

func (p *hostPreference) has(host string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.hosts[host]
	return ok
}

func (p *hostPreference) remember(host string) {
	if p == nil || host == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts[host] = struct{}{}
}

func (p *hostPreference) size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hosts)
}

// computeErrorClass 是 M2 的错误分类:调用方与运维据此区分
// "数据变了""节点坏了""对象不存在""格式不支持"。
type computeErrorClass string

const (
	classNone        computeErrorClass = ""
	classNotFound    computeErrorClass = "not_found"
	classStaleRange  computeErrorClass = "stale_range"
	classNodeError   computeErrorClass = "node_error"
	classUnsupported computeErrorClass = "unsupported_format"
)

// retryOnAnotherReplica 表示"换一个副本可能就好了"(节点故障、副本滞后)。
// stale_range 不属于此类:数据本身已改变,换副本通常没用,应由请求级
// 重新解析清单来处理(M2/T2.3)。
func (c computeErrorClass) retryOnAnotherReplica() bool {
	return c == classNodeError || c == classNotFound
}

// classifyComputeError 把错误归类。优先使用显式携带的分类(若上游通过
// 响应头或错误包装给出),否则按其文本中的关键字归类。
func classifyComputeError(err error) computeErrorClass {
	if err == nil {
		return classNone
	}
	if errors.Is(err, errStaleRange) {
		return classStaleRange
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "not found"), strings.Contains(text, "status 404"),
		strings.Contains(text, "404 "):
		return classNotFound
	case strings.Contains(text, "unsupported"), strings.Contains(text, "compressed"),
		strings.Contains(text, "manifest"):
		return classUnsupported
	default:
		return classNodeError
	}
}

// dataVersionOf 生成副本侧的版本描述串。
//
// 说明:SeaweedFS 的覆盖写会产生**新的 needle id**,因此"副本滞后"在本系统中
// 主要表现为"该副本上没有这个 needle"(not_found),而不是"同一个 needle 的旧内容"。
// 该串用于确认"副本实际服务的就是 filer 计划的那一个 needle",并结合
// 修改时间给出可比较的版本信号;M3 引入卷数据文件代次后会加入代次号。
func dataVersionOf(needleId uint64, cookie uint32, dataSize int64, lastModified int64) string {
	return fmt.Sprintf("%d:%d:%d:%d", needleId, cookie, dataSize, lastModified)
}

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
