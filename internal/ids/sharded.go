package ids

import "fmt"

// shardSuffixMarker separates a sharded ID's ULID part from its shard number.
const shardSuffixMarker = "-s"

// NewSharded returns New(prefix) with a shard suffix, such as "vm_01hx...-s0a". The suffix is "-s"
// followed by the shard as two lowercase hex digits, so every sharded ID still matches
// ^[a-zA-Z0-9_-]{1,64}$. The suffix sits at the end, so IDs still sort by creation time.
// NewSharded panics if shard is outside 0..255.
func NewSharded(prefix string, shard int) string {
	if shard < 0 || shard > 255 {
		panic(fmt.Sprintf("ids: shard %d out of range 0..255", shard))
	}
	return New(prefix) + shardSuffixMarker + fmt.Sprintf("%02x", shard)
}

// ShardOf reports the shard encoded in the suffix of id by NewSharded. ok is false when id has no
// well-formed suffix (for example IDs created by New, or IDs with upper-case or non-hex digits).
func ShardOf(id string) (shard int, ok bool) {
	const suffixLen = len("-s00")
	if len(id) <= suffixLen {
		return 0, false
	}
	suffix := id[len(id)-suffixLen:]
	if suffix[:len(shardSuffixMarker)] != shardSuffixMarker {
		return 0, false
	}
	hi, okHi := lowerHex(suffix[2])
	lo, okLo := lowerHex(suffix[3])
	if !okHi || !okLo {
		return 0, false
	}
	return hi<<4 | lo, true
}

// lowerHex decodes one lowercase hex digit.
func lowerHex(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	}
	return 0, false
}
