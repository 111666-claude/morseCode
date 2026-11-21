package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/go-audio/wav"
)

// Timing constants in Morse "units": dot = 1, dash = 3, intra-character gap = 1,
// letter gap = 3, word gap = 7. Classification boundaries sit halfway between
// the nominal ratios.
const (
	burstDotDashBoundary = 2.0 // burst < 2 units is a dot, >= 2 a dash
	gapLetterBoundary    = 2.0 // gap < 2 units is an element gap, >= 2 a letter gap
	gapWordBoundary      = 5.0 // gap >= 5 units is a word gap
	ambiguityMargin      = 0.5 // ratios within this distance of a boundary are flagged
	defaultMaxDuration   = 10 * time.Minute
)

// DecodeConfig holds optional decoding parameters.
type DecodeConfig struct {
	// WPM is the Morse speed in words per minute. The standard relation is
	// unit = 1.2/WPM seconds. Zero means the unit is estimated from the signal.
	WPM float64
	// MaxDuration rejects overlong recordings. Zero uses defaultMaxDuration.
	MaxDuration time.Duration
}

// LetterResult is the per-letter entry of a DecodeResult.
type LetterResult struct {
	Text       string  `json:"text"`
	Morse      string  `json:"morse"`
	Confidence float64 `json:"confidence"`
	Ambiguous  bool    `json:"ambiguous"`
}

// DecodeResult is the decoded text together with its confidence report.
type DecodeResult struct {
	Text                 string         `json:"text"`
	Morse                string         `json:"morse"`
	UnitSeconds          float64        `json:"unitSeconds"`
	WPM                  float64        `json:"wpm"`
	EstimatedUnitSeconds float64        `json:"estimatedUnitSeconds"`
	SNRdB                float64        `json:"snrDb"`
	Confidence           float64        `json:"confidence"`
	Warnings             []string       `json:"warnings"`
	Letters              []LetterResult `json:"letters"`
}

// morseToRune is the reverse of morseCodeDict.
var morseToRune = func() map[string]rune {
	m := make(map[string]rune, len(morseCodeDict))
	for r, code := range morseCodeDict {
		m[code] = r
	}
	return m
}()

// validate rejects non-finite and nonsensical parameters up front.
func (cfg DecodeConfig) validate() error {
	if math.IsNaN(cfg.WPM) || math.IsInf(cfg.WPM, 0) {
		return errors.New("wpm must be a finite number, got a non-finite value")
	}
	if cfg.WPM < 0 {
		return fmt.Errorf("wpm must be positive (or 0 for auto-estimate), got %v", cfg.WPM)
	}
	return nil
}

// DecodeWAV reads a mono PCM WAV Morse recording and returns the decoded text
// plus a confidence report. Hard structural problems (multi-channel, non-PCM,
// truncated or overlong files, invalid parameters) are returned as errors;
// uncertain timing is reported through low confidence and warnings.
func DecodeWAV(path string, cfg DecodeConfig) (*DecodeResult, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open WAV file: %w", err)
	}
	defer f.Close()

	d := wav.NewDecoder(f)
	d.ReadInfo()
	if err := d.Err(); err != nil {
		return nil, fmt.Errorf("invalid WAV file: %w", err)
	}
	if d.NumChans == 0 || d.BitDepth == 0 {
		return nil, errors.New("invalid WAV file: no readable fmt header")
	}
	if d.NumChans != 1 {
		return nil, fmt.Errorf("multi-channel WAV is not supported: %d channels found, mono PCM required", d.NumChans)
	}
	if d.WavAudioFormat != 1 {
		return nil, fmt.Errorf("unsupported WAV format tag %d: only uncompressed PCM is supported", d.WavAudioFormat)
	}
	switch d.BitDepth {
	case 8, 16, 24, 32:
	default:
		return nil, fmt.Errorf("unsupported PCM bit depth: %d (supported: 8, 16, 24, 32)", d.BitDepth)
	}
	if d.SampleRate == 0 {
		return nil, errors.New("invalid WAV header: sample rate is zero")
	}

	if err := d.FwdToPCM(); err != nil {
		return nil, fmt.Errorf("cannot locate PCM data: %w", err)
	}

	bytesPerFrame := int(d.BitDepth) / 8
	expectedFrames := int(d.PCMLen()) / bytesPerFrame
	duration := time.Duration(float64(expectedFrames) / float64(d.SampleRate) * float64(time.Second))
	maxDuration := cfg.MaxDuration
	if maxDuration <= 0 {
		maxDuration = defaultMaxDuration
	}
	if duration > maxDuration {
		return nil, fmt.Errorf("recording too long: %s exceeds limit of %s", duration.Round(time.Millisecond), maxDuration)
	}

	buf, err := d.FullPCMBuffer()
	if err != nil {
		return nil, fmt.Errorf("failed to read PCM samples: %w", err)
	}
	// A truncated data chunk reads fewer frames than its header declares.
	if gotFrames := len(buf.Data); gotFrames < expectedFrames {
		return nil, fmt.Errorf("truncated WAV file: header declares %d frames but only %d could be read", expectedFrames, gotFrames)
	}

	samples := normalizeSamples(buf.Data, d.BitDepth)
	return analyzeMorse(samples, int(d.SampleRate), int(d.BitDepth), cfg), nil
}

// normalizeSamples converts the decoder's integer samples to float64 in [-1, 1].
// 8-bit WAV samples are unsigned, the rest are signed.
func normalizeSamples(data []int, bitDepth uint16) []float64 {
	out := make([]float64, len(data))
	switch bitDepth {
	case 8:
		for i, v := range data {
			out[i] = (float64(v) - 128.0) / 128.0
		}
	case 16:
		for i, v := range data {
			out[i] = float64(v) / 32768.0
		}
	case 24:
		for i, v := range data {
			out[i] = float64(v) / 8388608.0
		}
	case 32:
		for i, v := range data {
			out[i] = float64(v) / 2147483648.0
		}
	}
	return out
}

type run struct {
	on         bool
	start, end int // frame indices, end exclusive
}

type symbolMark struct {
	symbol    byte // '.' or '-'
	ambiguous bool
}

// analyzeMorse turns normalized mono samples into text and a confidence report.
func analyzeMorse(samples []float64, sampleRate, bitDepth int, cfg DecodeConfig) *DecodeResult {
	res := &DecodeResult{Confidence: 1, Warnings: []string{}}

	frameLen := sampleRate / 100 // 10 ms RMS frames
	if frameLen < 8 {
		frameLen = 8
	}
	frameSec := float64(frameLen) / float64(sampleRate)
	env := frameRMS(samples, frameLen)
	if len(env) == 0 {
		res.Confidence = 0
		res.Warnings = append(res.Warnings, "no audio samples found")
		return res
	}

	sorted := append([]float64(nil), env...)
	sort.Float64s(sorted)
	// Silence frames (gaps, leading/trailing quiet) dominate long recordings,
	// so the tone level is read near the top of the RMS distribution.
	noise := percentileSorted(sorted, 0.10)
	signal := percentileSorted(sorted, 0.99)
	if signal < 1e-5 {
		res.Confidence = 0
		res.Warnings = append(res.Warnings, "no tone detected: recording appears to contain only silence")
		return res
	}

	// SNR is measured at sample level inside quiet frames: frame RMS averages
	// zero-mean background noise down and would overstate the ratio. The noise
	// floor is the median absolute sample in quiet frames, clamped to the
	// format's digital floor.
	digitalFloor := 1.0 / math.Pow(2, float64(bitDepth-1))
	var quietSamples []float64
	quietThreshold := noise + 0.25*(signal-noise)
	for f, level := range env {
		if level <= quietThreshold {
			base := f * frameLen
			for j := 0; j < frameLen && base+j < len(samples); j++ {
				quietSamples = append(quietSamples, math.Abs(samples[base+j]))
			}
		}
	}
	noiseLevel := digitalFloor
	if len(quietSamples) > 0 {
		noiseLevel = math.Max(median(quietSamples), digitalFloor)
	}
	res.SNRdB = 20 * math.Log10(signal/noiseLevel)
	switch {
	case res.SNRdB < 6:
		res.Warnings = append(res.Warnings, fmt.Sprintf("very low signal-to-noise ratio: %.1f dB", res.SNRdB))
		res.Confidence *= 0.6
	case res.SNRdB < 10:
		res.Warnings = append(res.Warnings, fmt.Sprintf("low signal-to-noise ratio: %.1f dB", res.SNRdB))
		res.Confidence *= 0.8
	}

	runs := detectRuns(env, noise, signal)
	var burstDurs []float64
	for _, r := range runs {
		if r.on {
			burstDurs = append(burstDurs, float64(r.end-r.start))
		}
	}
	if len(burstDurs) == 0 {
		res.Confidence = 0
		res.Warnings = append(res.Warnings, "no tone bursts detected above the noise floor")
		return res
	}

	autoUnitFrames := estimateUnit(burstDurs)
	res.EstimatedUnitSeconds = autoUnitFrames * frameSec

	var unitFrames float64
	if cfg.WPM > 0 {
		res.UnitSeconds = 1.2 / cfg.WPM
		unitFrames = res.UnitSeconds / frameSec
		if cfg.WPM < 2 || cfg.WPM > 60 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("configured WPM %v is outside the usual 2-60 range", cfg.WPM))
		}
		if ratio := autoUnitFrames / unitFrames; ratio > 1.5 || ratio < 1.0/1.5 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("configured WPM does not match observed timing (estimated unit %.3fs vs configured %.3fs)",
				res.EstimatedUnitSeconds, res.UnitSeconds))
			res.Confidence *= 0.8
		}
	} else {
		unitFrames = autoUnitFrames
		res.UnitSeconds = res.EstimatedUnitSeconds
	}
	res.WPM = 1.2 / res.UnitSeconds
	if res.UnitSeconds < 0.02 || res.UnitSeconds > 2.0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("estimated unit duration %.3fs is outside the plausible 0.02-2s range", res.UnitSeconds))
		res.Confidence *= 0.7
	}

	groups, wordAfter, ambiguousGaps := classifyRuns(runs, unitFrames)

	letters := make([]LetterResult, 0, len(groups))
	var textParts, morseParts []string
	for i, group := range groups {
		var codeBuf []byte
		ambiguous := false
		for _, m := range group {
			codeBuf = append(codeBuf, m.symbol)
			if m.ambiguous {
				ambiguous = true
			}
		}
		code := string(codeBuf)
		lr := LetterResult{Morse: code, Ambiguous: ambiguous}
		if r, ok := morseToRune[code]; ok {
			lr.Text = string(r)
			lr.Confidence = 1.0
			if ambiguous {
				lr.Confidence = 0.5
			}
		} else {
			lr.Text = "?"
			lr.Confidence = 0.25
			res.Warnings = append(res.Warnings, fmt.Sprintf("undecodable Morse sequence %q", code))
		}
		letters = append(letters, lr)
		textParts = append(textParts, lr.Text)
		morseParts = append(morseParts, code)
		if wordAfter[i] {
			textParts = append(textParts, " ")
			morseParts = append(morseParts, "/")
		}
	}
	res.Letters = letters
	res.Text = joinWords(textParts)
	res.Morse = joinMorse(morseParts)

	if ambiguousGaps > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d gap(s) lie close to a letter/word timing boundary", ambiguousGaps))
		res.Confidence *= math.Max(0.6, 1.0-0.1*float64(ambiguousGaps))
	}

	var sum float64
	for _, l := range letters {
		sum += l.Confidence
	}
	if len(letters) > 0 {
		res.Confidence *= sum / float64(len(letters))
	} else {
		res.Confidence = 0
	}
	if res.Confidence > 1 {
		res.Confidence = 1
	}
	res.Confidence = math.Round(res.Confidence*1000) / 1000
	return res
}

// joinWords joins single-letter tokens, keeping the explicit spaces as word gaps.
func joinWords(parts []string) string {
	var b []byte
	for _, p := range parts {
		if p == " " {
			if len(b) > 0 && b[len(b)-1] != ' ' {
				b = append(b, ' ')
			}
			continue
		}
		b = append(b, p...)
	}
	return string(b)
}

// joinMorse joins Morse groups with a space, using " / " at word boundaries.
func joinMorse(parts []string) string {
	var b []byte
	for _, p := range parts {
		if p == "/" {
			if len(b) > 0 && b[len(b)-1] == ' ' {
				b = b[:len(b)-1]
			}
			b = append(b, ' ', '/', ' ')
			continue
		}
		if len(b) > 0 && b[len(b)-1] != ' ' {
			b = append(b, ' ')
		}
		b = append(b, p...)
	}
	return string(b)
}

// classifyRuns converts on/off runs into Morse symbol groups, returning the
// groups and the set of group indices followed by a word gap.
func classifyRuns(runs []run, unitFrames float64) (groups [][]symbolMark, wordAfter map[int]bool, ambiguousGaps int) {
	wordAfter = map[int]bool{}
	var cur []symbolMark
	seenTone := false

	for idx, r := range runs {
		dur := float64(r.end-r.start) / unitFrames
		if r.on {
			mark := symbolMark{symbol: '.'}
			if dur >= burstDotDashBoundary {
				mark.symbol = '-'
			}
			if math.Abs(dur-burstDotDashBoundary) < ambiguityMargin {
				mark.ambiguous = true
			}
			cur = append(cur, mark)
			seenTone = true
			continue
		}
		if !seenTone {
			continue // leading silence
		}
		hasLaterTone := false
		for _, q := range runs[idx+1:] {
			if q.on {
				hasLaterTone = true
				break
			}
		}
		if !hasLaterTone {
			break // trailing silence
		}

		switch {
		case dur < gapLetterBoundary:
			if dur > gapLetterBoundary-ambiguityMargin {
				ambiguousGaps++
			}
		case dur < gapWordBoundary:
			if math.Abs(dur-gapLetterBoundary) < ambiguityMargin ||
				math.Abs(dur-gapWordBoundary) < ambiguityMargin {
				ambiguousGaps++
			}
			groups = append(groups, cur)
			cur = nil
		default:
			if math.Abs(dur-gapWordBoundary) < ambiguityMargin {
				ambiguousGaps++
			}
			if dur > 12 {
				ambiguousGaps++
			}
			groups = append(groups, cur)
			wordAfter[len(groups)-1] = true
			cur = nil
		}
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups, wordAfter, ambiguousGaps
}

// detectRuns labels RMS frames on/off with hysteresis and then debounces runs
// shorter than two frames so isolated noise frames do not create fake symbols.
func detectRuns(env []float64, noise, signal float64) []run {
	span := signal - noise
	onThreshold := noise + 0.5*span
	offThreshold := noise + 0.25*span

	labels := make([]bool, len(env))
	on := false
	for i, level := range env {
		switch {
		case !on && level >= onThreshold:
			on = true
		case on && level < offThreshold:
			on = false
		}
		labels[i] = on
	}

	var runs []run
	start := 0
	for i := 1; i <= len(labels); i++ {
		if i == len(labels) || labels[i] != labels[start] {
			runs = append(runs, run{on: labels[start], start: start, end: i})
			start = i
		}
	}

	const minRun = 2
	for {
		idx := -1
		for i, r := range runs {
			if r.end-r.start < minRun {
				idx = i
				break
			}
		}
		if idx < 0 {
			return runs
		}
		switch {
		case idx > 0:
			runs[idx-1].end = runs[idx].end
			runs = append(runs[:idx], runs[idx+1:]...)
		case len(runs) > 1:
			runs[1].start = runs[0].start
			runs = runs[1:]
		default:
			return runs
		}
		var merged []run
		for _, r := range runs {
			if n := len(merged); n > 0 && merged[n-1].on == r.on {
				merged[n-1].end = r.end
			} else {
				merged = append(merged, r)
			}
		}
		runs = merged
	}
}

// estimateUnit derives the dot duration (in frames) from the observed bursts.
// Dots are the shortest stable bursts, so the lower quartile seeds the search
// and the median of near-dot bursts gives a robust estimate.
func estimateUnit(burstDurs []float64) float64 {
	sorted := append([]float64(nil), burstDurs...)
	sort.Float64s(sorted)
	seed := percentileSorted(sorted, 0.25)
	var dots []float64
	for _, v := range sorted {
		if v < 1.8*seed {
			dots = append(dots, v)
		}
	}
	if len(dots) == 0 {
		return seed
	}
	return median(dots)
}

// frameRMS computes the RMS energy of non-overlapping frames.
func frameRMS(samples []float64, frameLen int) []float64 {
	n := len(samples) / frameLen
	out := make([]float64, n)
	for f := 0; f < n; f++ {
		base := f * frameLen
		var sum float64
		for j := 0; j < frameLen; j++ {
			v := samples[base+j]
			sum += v * v
		}
		out[f] = math.Sqrt(sum / float64(frameLen))
	}
	return out
}

func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func median(values []float64) float64 {
	sort.Float64s(values)
	return percentileSorted(values, 0.5)
}

// runDecode is the CLI entry point for decode mode.
func runDecode(path string, cfg DecodeConfig, asJSON bool) error {
	res, err := DecodeWAV(path, cfg)
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	fmt.Println(res.Text)
	fmt.Fprintf(os.Stderr, "Morse code:  %s\n", res.Morse)
	fmt.Fprintf(os.Stderr, "Unit:        %.3fs (%.1f WPM)", res.UnitSeconds, res.WPM)
	if res.EstimatedUnitSeconds > 0 && cfg.WPM == 0 {
		fmt.Fprint(os.Stderr, ", auto-estimated")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "SNR:         %.1f dB\n", res.SNRdB)
	fmt.Fprintf(os.Stderr, "Confidence:  %.3f\n", res.Confidence)
	if len(res.Warnings) > 0 {
		fmt.Fprintln(os.Stderr, "Warnings:")
		for _, w := range res.Warnings {
			fmt.Fprintf(os.Stderr, "  - %s\n", w)
		}
	}
	return nil
}
