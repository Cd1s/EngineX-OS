package analyzer

import (
	"fmt"
	"math"
)

// Key 是检测出的调性。EngineID 对应 Engine DJ 数据库中 Track.key 的取值
// （按五度圈排列，大调为偶数、其关系小调为下一个奇数）。
type Key struct {
	EngineID int    `json:"engineId"`
	Name     string `json:"name"`
	Camelot  string `json:"camelot"`
}

var pitchNames = [12]string{"C", "C#", "D", "Eb", "E", "F", "F#", "G", "Ab", "A", "Bb", "B"}

// Krumhansl-Kessler 调性模板。
var (
	majorProfile = [12]float64{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88}
	minorProfile = [12]float64{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17}
)

// KeyFromEngineID 由 Engine key id（0–23）构造 Key。
func KeyFromEngineID(id int) Key {
	if id < 0 || id > 23 {
		return Key{EngineID: -1}
	}
	step := id / 2 // 五度圈上距 C 大调的步数
	minor := id%2 == 1
	root := (7 * step) % 12
	name := pitchNames[root]
	letter := "B"
	if minor {
		root = (root + 9) % 12
		name = pitchNames[root] + "m"
		letter = "A"
	}
	return Key{EngineID: id, Name: name, Camelot: fmt.Sprintf("%d%s", (7+step)%12+1, letter)}
}

// keyFromRoot 把（主音音级, 是否小调）转换为 Key。
func keyFromRoot(root int, minor bool) Key {
	major := root
	if minor {
		major = (root + 3) % 12 // 关系大调
	}
	step := (major * 7) % 12 // 五度圈步数（7 是 7 关于模 12 的逆元）
	id := step * 2
	if minor {
		id++
	}
	return KeyFromEngineID(id)
}

const (
	keyFFT     = 4096
	keyHop     = 2048
	keyMinFreq = 100
	keyMaxFreq = 2000
)

// detectKey 计算全曲 chroma，与 24 个调性模板求 Pearson 相关，取最大者。
// 置信度为最佳与次佳相关系数之差。
func detectKey(x []float32, sr int, tick func(int) error) (Key, float64, error) {
	s := newSTFT(keyFFT, keyHop)
	binPC := make([]int, len(s.mag))
	for b := range binPC {
		f := float64(b) * float64(sr) / keyFFT
		if f < keyMinFreq || f > keyMaxFreq {
			binPC[b] = -1
			continue
		}
		midi := int(math.Round(69 + 12*math.Log2(f/440)))
		binPC[b] = ((midi % 12) + 12) % 12
	}
	var chroma [12]float64
	nf := s.frames(len(x))
	for i := 0; i < nf; i++ {
		mag := s.frame(x, i)
		var frame [12]float64
		total := 0.0
		for b, pc := range binPC {
			if pc >= 0 {
				e := mag[b] * mag[b]
				frame[pc] += e
				total += e
			}
		}
		// 帧内归一化，使响亮段落不会淹没安静段落的和声信息。
		if total > 0 {
			for pc := range frame {
				chroma[pc] += math.Sqrt(frame[pc] / total)
			}
		}
		if err := tick(i * keyHop); err != nil {
			return Key{}, 0, err
		}
	}

	best, second := -2.0, -2.0
	var bestKey Key
	for root := 0; root < 12; root++ {
		for _, minor := range []bool{false, true} {
			prof := majorProfile
			if minor {
				prof = minorProfile
			}
			var rotated [12]float64
			for i := 0; i < 12; i++ {
				rotated[(i+root)%12] = prof[i]
			}
			c := pearson(chroma[:], rotated[:])
			if c > best {
				second = best
				best, bestKey = c, keyFromRoot(root, minor)
			} else if c > second {
				second = c
			}
		}
	}
	return bestKey, best - second, nil
}

func pearson(a, b []float64) float64 {
	var ma, mb float64
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(len(a))
	mb /= float64(len(b))
	var num, da, db float64
	for i := range a {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}
