package engine

import (
	"bufio"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

type candidateRange struct {
	raw      string
	family   string
	base     net.IP
	net      *net.IPNet
	prefix   int
	bits     int
	capacity uint64 // saturated at math.MaxUint64 for huge IPv6 ranges
}

func parseCandidateRanges(path, family string) ([]candidateRange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []candidateRange
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		cidr := line
		if !strings.Contains(cidr, "/") {
			if family == "ipv4" {
				cidr += "/32"
			} else {
				cidr += "/128"
			}
		}
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", line, err)
		}
		is4 := ip.To4() != nil
		if family == "ipv4" && !is4 {
			continue
		}
		if family == "ipv6" && is4 {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		hostBits := bits - ones
		cap := uint64(math.MaxUint64)
		if hostBits < 64 {
			cap = uint64(1) << uint(hostBits)
		}
		base := ip.Mask(ipnet.Mask)
		if is4 {
			base = base.To4()
		} else {
			base = base.To16()
		}
		out = append(out, candidateRange{raw: line, family: family, base: append(net.IP(nil), base...), net: ipnet, prefix: ones, bits: bits, capacity: cap})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid %s ranges", family)
	}
	return out, nil
}

func clampSampleCount(ranges []candidateRange, requested, hardMax int) int {
	if hardMax <= 0 {
		hardMax = 10000
	}
	if requested <= 0 {
		requested = 256
	}
	if requested > hardMax {
		requested = hardMax
	}
	var finite uint64
	allFinite := true
	for _, r := range ranges {
		if r.capacity == math.MaxUint64 {
			allFinite = false
			break
		}
		if math.MaxUint64-finite < r.capacity {
			allFinite = false
			break
		}
		finite += r.capacity
	}
	if allFinite && finite < uint64(requested) {
		return int(finite)
	}
	return requested
}

func allocateSamples(ranges []candidateRange, total int) []int {
	alloc := make([]int, len(ranges))
	if total <= 0 || len(ranges) == 0 {
		return alloc
	}
	remaining := total
	active := make([]bool, len(ranges))
	for i := range active {
		active[i] = true
	}
	for remaining > 0 {
		activeCount := 0
		for i := range ranges {
			if active[i] {
				activeCount++
			}
		}
		if activeCount == 0 {
			break
		}
		base := remaining / activeCount
		if base == 0 {
			base = 1
		}
		progress := false
		for i, r := range ranges {
			if !active[i] || remaining <= 0 {
				continue
			}
			want := base
			if want > remaining {
				want = remaining
			}
			if r.capacity != math.MaxUint64 {
				used := uint64(alloc[i])
				if used >= r.capacity {
					active[i] = false
					continue
				}
				left := r.capacity - used
				if uint64(want) > left {
					want = int(left)
				}
			}
			if want <= 0 {
				active[i] = false
				continue
			}
			alloc[i] += want
			remaining -= want
			progress = true
			if r.capacity != math.MaxUint64 && uint64(alloc[i]) >= r.capacity {
				active[i] = false
			}
		}
		if !progress {
			break
		}
	}
	return alloc
}

func sampleCandidateFile(rawPath, outPath, family string, requested, hardMax int) (int, []string, error) {
	ranges, err := parseCandidateRanges(rawPath, family)
	if err != nil {
		return 0, nil, err
	}
	total := clampSampleCount(ranges, requested, hardMax)
	alloc := allocateSamples(ranges, total)

	seedBytes := make([]byte, 8)
	_, _ = crand.Read(seedBytes)
	seed := int64(binary.LittleEndian.Uint64(seedBytes)) ^ time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))

	seen := map[string]bool{}
	ips := make([]string, 0, total)
	allocationNotes := make([]string, 0, len(ranges))
	for i, r := range ranges {
		want := alloc[i]
		allocationNotes = append(allocationNotes, fmt.Sprintf("%s=%d", r.raw, want))
		generated := sampleRange(r, want, rng)
		for _, ip := range generated {
			if !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	// Overlapping ranges can produce duplicates. Refill from ranges round-robin.
	attempts := 0
	for len(ips) < total && attempts < total*20 {
		r := ranges[attempts%len(ranges)]
		for _, ip := range sampleRange(r, 1, rng) {
			if !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
		attempts++
	}
	if len(ips) == 0 {
		return 0, allocationNotes, fmt.Errorf("sampling produced no %s candidates", family)
	}
	// Randomize cross-range ordering so one range does not always dominate the first workers.
	rng.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	f, err := os.Create(outPath)
	if err != nil {
		return 0, allocationNotes, err
	}
	w := bufio.NewWriter(f)
	for _, ip := range ips {
		if _, err := fmt.Fprintln(w, ip); err != nil {
			f.Close()
			return 0, allocationNotes, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, allocationNotes, err
	}
	if err := f.Close(); err != nil {
		return 0, allocationNotes, err
	}
	return len(ips), allocationNotes, nil
}

func sampleRange(r candidateRange, count int, rng *rand.Rand) []string {
	if count <= 0 {
		return nil
	}
	if r.family == "ipv4" {
		return sampleIPv4Range(r, count, rng)
	}
	return sampleIPv6Range(r, count, rng)
}

func sampleIPv4Range(r candidateRange, count int, rng *rand.Rand) []string {
	base := binary.BigEndian.Uint32(r.base.To4())
	cap := r.capacity
	if uint64(count) > cap {
		count = int(cap)
	}
	offsets := make([]uint32, 0, count)
	if cap <= 1_000_000 && count > int(cap)/3 {
		perm := rng.Perm(int(cap))
		for _, v := range perm[:count] {
			offsets = append(offsets, uint32(v))
		}
	} else {
		seen := map[uint32]bool{}
		for len(offsets) < count {
			var off uint32
			if cap == math.MaxUint64 || cap > math.MaxUint32 {
				off = rng.Uint32()
			} else {
				off = uint32(rng.Int63n(int64(cap)))
			}
			if !seen[off] {
				seen[off] = true
				offsets = append(offsets, off)
			}
		}
	}
	out := make([]string, 0, len(offsets))
	for _, off := range offsets {
		v := base + off
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, v)
		out = append(out, ip.String())
	}
	return out
}

func sampleIPv6Range(r candidateRange, count int, rng *rand.Rand) []string {
	hostBits := r.bits - r.prefix
	if r.capacity != math.MaxUint64 && uint64(count) > r.capacity {
		count = int(r.capacity)
	}
	out := make([]string, 0, count)
	seen := map[string]bool{}
	// Small ranges can be enumerated exactly and then shuffled.
	if hostBits <= 20 && r.capacity <= 1_000_000 && count > int(r.capacity)/3 {
		vals := make([]uint64, int(r.capacity))
		for i := range vals {
			vals[i] = uint64(i)
		}
		rng.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
		for _, off := range vals[:count] {
			ip := addIPv6Offset(r.base, off)
			out = append(out, ip.String())
		}
		return out
	}
	for len(out) < count {
		ip := append(net.IP(nil), r.base.To16()...)
		hostBytes := (hostBits + 7) / 8
		randBytes := make([]byte, hostBytes)
		_, _ = crand.Read(randBytes)
		start := 16 - hostBytes
		copy(ip[start:], randBytes)
		// Preserve network bits in a partially covered byte.
		if rem := r.prefix % 8; rem != 0 {
			mask := byte(0xff << uint(8-rem))
			idx := r.prefix / 8
			ip[idx] = (r.base[idx] & mask) | (ip[idx] & ^mask)
		}
		ip = ip.Mask(r.net.Mask)
		// Re-apply random host bits after Mask zeroes them.
		copy(ip[start:], randBytes)
		if rem := r.prefix % 8; rem != 0 {
			mask := byte(0xff << uint(8-rem))
			idx := r.prefix / 8
			ip[idx] = (r.base[idx] & mask) | (ip[idx] & ^mask)
		}
		if !r.net.Contains(ip) {
			continue
		}
		key := ip.String()
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func addIPv6Offset(base net.IP, off uint64) net.IP {
	ip := append(net.IP(nil), base.To16()...)
	for i := 15; i >= 0 && off > 0; i-- {
		sum := uint64(ip[i]) + (off & 0xff)
		ip[i] = byte(sum)
		off = (off >> 8) + (sum >> 8)
	}
	return ip
}

// estimateLocalCapacity is used by tests and API diagnostics. Huge IPv6 pools are
// reported as saturated rather than attempting arbitrary-precision expansion.
func estimateLocalCapacity(path, family string) (uint64, int, error) {
	ranges, err := parseCandidateRanges(path, family)
	if err != nil {
		return 0, 0, err
	}
	var total uint64
	for _, r := range ranges {
		if r.capacity == math.MaxUint64 || math.MaxUint64-total < r.capacity {
			return math.MaxUint64, len(ranges), nil
		}
		total += r.capacity
	}
	return total, len(ranges), nil
}

func sortedAllocationNotes(notes []string) []string {
	out := append([]string(nil), notes...)
	sort.Strings(out)
	return out
}
