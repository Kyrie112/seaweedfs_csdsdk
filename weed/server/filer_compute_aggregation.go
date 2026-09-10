package weed_server

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

const (
	// computeChunkPackingUnit is the word size used by the device-side packing
	// operators: the sum kernel packs every 8 bytes into one little-endian
	// uint64 word before adding. Per-chunk results only stay congruent with the
	// whole-file result when every chunk is a multiple of this unit, because a
	// chunk boundary in the middle of a word changes how the two halves are
	// packed.
	computeChunkPackingUnit = 8

	// imageSearchOperation is the near-storage top-k similarity search operator.
	// Each chunk returns its own partial top-k list, and the filer merges them.
	imageSearchOperation = "img_topk"

	defaultImageTopK = 5
	maxImageTopK     = 1000
)

// searchHit is one entry of a partial top-k list.
type searchHit struct {
	index    int64
	distance int64
}

// aggregateAdditiveResults combines per-chunk results of an additive numeric
// operator.
//
// Each device result is a 64-bit value, so the consistent aggregate is their
// sum *reduced modulo 2^64*. Reducing matters: without it a multi-chunk request
// could return a value larger than 2^64 for exactly the same bytes that a
// single-chunk request reports as a 64-bit value, i.e. the operator's codomain
// would depend on how the file happens to be split into chunks.
func aggregateAdditiveResults(bodies [][]byte) (string, error) {
	modulus := new(big.Int).Lsh(big.NewInt(1), 64)
	total := new(big.Int)
	for i, body := range bodies {
		text := strings.TrimSpace(string(body))
		value, ok := new(big.Int).SetString(text, 10)
		if !ok {
			snippet := text
			if len(snippet) > 128 {
				snippet = snippet[:128]
			}
			return "", fmt.Errorf("cross-chunk additive aggregation requires numeric per-chunk results, chunk %d returned (truncated): %q", i, snippet)
		}
		total.Add(total, value)
	}
	total.Mod(total, modulus)
	return total.String(), nil
}

// mergeTopKResults merges the partial top-k lists returned by the near-storage
// search operator. Merging is the "holistic" half of the operator algebra: the
// per-chunk results are not additive, but each chunk can still prune locally as
// long as it reports its own top k, because the global top k is always a subset
// of the union of the per-chunk top k lists.
func mergeTopKResults(bodies [][]byte, k int) (string, error) {
	if k <= 0 {
		k = defaultImageTopK
	}
	candidates := make([]searchHit, 0, len(bodies)*k)
	for i, body := range bodies {
		hits, err := parsePartialTopK(string(body))
		if err != nil {
			return "", fmt.Errorf("chunk %d: %w", i, err)
		}
		candidates = append(candidates, hits...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance < candidates[j].distance
		}
		return candidates[i].index < candidates[j].index
	})
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	var out strings.Builder
	for i, hit := range candidates {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.FormatInt(hit.index, 10))
		out.WriteByte(':')
		out.WriteString(strconv.FormatInt(hit.distance, 10))
	}
	return out.String(), nil
}

// parsePartialTopK parses "<index>:<distance>" pairs ordered by ascending
// distance, as produced by the device-side search operator.
func parsePartialTopK(body string) ([]searchHit, error) {
	text := strings.TrimSpace(body)
	if text == "" {
		return nil, nil
	}
	entries := strings.Split(text, ",")
	hits := make([]searchHit, 0, len(entries))
	for _, entry := range entries {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed search hit %q", entry)
		}
		index, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed search hit index %q: %w", parts[0], err)
		}
		distance, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed search hit distance %q: %w", parts[1], err)
		}
		hits = append(hits, searchHit{index: index, distance: distance})
	}
	return hits, nil
}

// imageTopKFromQuery reads the requested k, clamped to a sane range.
func imageTopKFromQuery(raw string) int {
	if raw == "" {
		return defaultImageTopK
	}
	k, err := strconv.Atoi(raw)
	if err != nil || k <= 0 {
		return defaultImageTopK
	}
	if k > maxImageTopK {
		return maxImageTopK
	}
	return k
}
