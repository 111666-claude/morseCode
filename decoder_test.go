package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// writeMonoWAV writes normalized float samples as a 16-bit mono PCM WAV file.
func writeMonoWAV(t *testing.T, path string, samples []float64, sampleRate, bitDepth int, channels int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	enc := wav.NewEncoder(f, sampleRate, bitDepth, channels, 1)
	n := len(samples)
	if channels == 2 {
		n /= 2
	}
	buf := &audio.IntBuffer{
		Data:   make([]int, n*channels),
		Format: &audio.Format{SampleRate: sampleRate, NumChannels: channels},
	}
	scale := float64(int(1) << (bitDepth - 1))
	for i, v := range samples {
		s := int(v * scale)
		if s > int(scale)-1 {
			s = int(scale) - 1
		}
		if s < -int(scale) {
			s = -int(scale)
		}
		buf.Data[i] = s
	}
	if err := enc.Write(buf); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close wav: %v", err)
	}
}

// encodeMorseSamples reuses the production encoder's tone/silence primitives so
// the round-trip tests exercise the exact same generation path.
func encodeMorseSamples(text string, sampleRate float64) []float64 {
	morseCode := textToMorse(text)
	dotDuration := 0.1
	dashDuration := 3 * dotDuration
	frequency := 1000.0
	amplitude := 0.5

	var seq []float64
	for _, char := range morseCode {
		switch char {
		case '.':
			seq = append(seq, generateTone(frequency, dotDuration, sampleRate, amplitude)...)
		case '-':
			seq = append(seq, generateTone(frequency, dashDuration, sampleRate, amplitude)...)
		case ' ':
			seq = append(seq, appendSilence(dotDuration, sampleRate)...)
		}
		seq = append(seq, appendSilence(dotDuration, sampleRate)...)
	}
	return seq
}

// addFixedNoise overlays reproducible pseudo-random noise (fixed seed) and a
// volume change, and pads with leading/trailing silence.
func addFixedNoise(samples []float64, rng *rand.Rand, noiseLevel, volume float64, leadSilence, tailSilence int) []float64 {
	out := make([]float64, 0, leadSilence+len(samples)+tailSilence)
	out = append(out, make([]float64, leadSilence)...)
	for _, s := range samples {
		n := (rng.Float64()*2 - 1) * noiseLevel
		out = append(out, s*volume+n)
	}
	out = append(out, make([]float64, tailSilence)...)
	return out
}

// trimFileBytes truncates the raw file on disk so the data chunk ends early.
func trimFileBytes(t *testing.T, path string, cut int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if cut >= len(b) {
		cut = len(b) / 2
	}
	if err := os.WriteFile(path, b[:len(b)-cut], 0o644); err != nil {
		t.Fatalf("truncate fixture: %v", err)
	}
}

func TestRoundTripClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.wav")
	samples := encodeMorseSamples("SOS", 44100)
	writeMonoWAV(t, path, samples, 44100, 16, 1)

	res, err := DecodeWAV(path, DecodeConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "SOS" {
		t.Fatalf("decoded text = %q, want SOS; morse=%s warnings=%v", res.Text, res.Morse, res.Warnings)
	}
	if res.Morse != "... --- ..." {
		t.Fatalf("morse = %q", res.Morse)
	}
	if res.Confidence < 0.99 {
		t.Fatalf("clean signal confidence too low: %.3f warnings=%v", res.Confidence, res.Warnings)
	}
}

func TestRoundTripAlphanumeric(t *testing.T) {
	dir := t.TempDir()
	for _, text := range []string{"HELLO", "MORSE123", "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"} {
		path := filepath.Join(dir, "rt_"+text+".wav")
		samples := encodeMorseSamples(text, 44100)
		writeMonoWAV(t, path, samples, 44100, 16, 1)

		res, err := DecodeWAV(path, DecodeConfig{})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", text, err)
		}
		if res.Text != text {
			t.Fatalf("%s: decoded %q, want %q (warnings=%v)", text, res.Text, text, res.Warnings)
		}
	}
}

func TestRoundTripWithNoiseSilenceAndVolumeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noisy.wav")
	clean := encodeMorseSamples("SOS", 44100)
	rng := rand.New(rand.NewSource(42)) // fixed seed: reproducible noise
	// 5% noise, half volume, 0.3s leading and 0.5s trailing silence.
	noisy := addFixedNoise(clean, rng, 0.05, 0.5, 13230, 22050)
	writeMonoWAV(t, path, noisy, 44100, 16, 1)

	res, err := DecodeWAV(path, DecodeConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "SOS" {
		t.Fatalf("noisy decode = %q, want SOS; warnings=%v", res.Text, res.Warnings)
	}
	if res.Confidence < 0.95 {
		t.Fatalf("noisy signal confidence too low: %.3f warnings=%v", res.Confidence, res.Warnings)
	}

	// Explicit WPM configuration (100 ms dot => unit 0.1s => 12 WPM) must
	// produce the same text.
	resCfg, err := DecodeWAV(path, DecodeConfig{WPM: 12})
	if err != nil {
		t.Fatalf("configured decode error: %v", err)
	}
	if resCfg.Text != "SOS" {
		t.Fatalf("configured WPM decode = %q, want SOS", resCfg.Text)
	}
}

func TestRoundTrip8Bit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eight.wav")
	samples := encodeMorseSamples("E", 44100)
	writeMonoWAV(t, path, samples, 44100, 8, 1)

	res, err := DecodeWAV(path, DecodeConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "E" {
		t.Fatalf("8-bit decode = %q, want E; warnings=%v", res.Text, res.Warnings)
	}
}

func TestWordGapDecodedAsSpace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "words.wav")
	sr := 44100.0
	unit := 0.1
	// "E T": dot, full 7-unit word gap, dash.
	seq := append(generateTone(1000, unit, sr, 0.5), appendSilence(7*unit, sr)...)
	seq = append(seq, generateTone(1000, 3*unit, sr, 0.5)...)
	writeMonoWAV(t, path, seq, 44100, 16, 1)

	res, err := DecodeWAV(path, DecodeConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "E T" {
		t.Fatalf("word-gap decode = %q, want %q; warnings=%v", res.Text, "E T", res.Warnings)
	}
	if res.Morse != ". / -" {
		t.Fatalf("morse with word gap = %q", res.Morse)
	}
}

func TestStereoRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stereo.wav")
	samples := encodeMorseSamples("E", 44100)
	stereo := make([]float64, len(samples)*2)
	for i, s := range samples {
		stereo[2*i] = s
		stereo[2*i+1] = s
	}
	writeMonoWAV(t, path, stereo, 44100, 16, 2)

	if _, err := DecodeWAV(path, DecodeConfig{}); err == nil {
		t.Fatal("expected error for stereo WAV, got nil")
	} else if !strings.Contains(err.Error(), "channel") {
		t.Fatalf("error should mention channels, got: %v", err)
	}
}

func TestTruncatedFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated.wav")
	samples := encodeMorseSamples("SOS", 44100)
	writeMonoWAV(t, path, samples, 44100, 16, 1)
	trimFileBytes(t, path, 2048)

	if _, err := DecodeWAV(path, DecodeConfig{}); err == nil {
		t.Fatal("expected error for truncated WAV, got nil")
	} else if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error should mention truncation, got: %v", err)
	}
}

func TestOverlongRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.wav")
	samples := encodeMorseSamples("E", 44100)
	writeMonoWAV(t, path, samples, 44100, 16, 1)

	_, err := DecodeWAV(path, DecodeConfig{MaxDuration: 10 * time.Millisecond})
	if err == nil {
		t.Fatal("expected error for overlong recording, got nil")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("error should mention length limit, got: %v", err)
	}
}

func TestNonFiniteAndNegativeParameters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.wav")
	samples := encodeMorseSamples("E", 44100)
	writeMonoWAV(t, path, samples, 44100, 16, 1)

	for _, wpm := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		if _, err := DecodeWAV(path, DecodeConfig{WPM: wpm}); err == nil {
			t.Fatalf("expected error for WPM=%v", wpm)
		}
	}
}

func TestAmbiguousTimingMarkedLowConfidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ambiguous.wav")
	sr := 44100.0
	unit := 0.1
	// Hand-built signal: one real 1-unit dot anchors the estimator, then a
	// ~2.1-unit burst sits inside the dot/dash ambiguity band around 2 units,
	// with 1-unit element gaps between them.
	ambiguous := append(generateTone(1000, unit, sr, 0.5), appendSilence(unit, sr)...)
	ambiguous = append(ambiguous, generateTone(1000, 2.1*unit, sr, 0.5)...)
	ambiguous = append(ambiguous, appendSilence(unit, sr)...)
	ambiguous = append(ambiguous, generateTone(1000, unit, sr, 0.5)...)
	writeMonoWAV(t, path, ambiguous, 44100, 16, 1)

	res, err := DecodeWAV(path, DecodeConfig{})
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !hasAmbiguousLetter(res) {
		t.Fatalf("expected at least one ambiguous letter, got %+v", res.Letters)
	}
	if res.Confidence > 0.9 {
		t.Fatalf("expected reduced confidence for ambiguous timing, got %.3f", res.Confidence)
	}
}

func hasAmbiguousLetter(res *DecodeResult) bool {
	for _, l := range res.Letters {
		if l.Ambiguous {
			return true
		}
	}
	return false
}

func TestSilenceIsLowConfidenceNotCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "silence.wav")
	writeMonoWAV(t, path, make([]float64, 44100), 44100, 16, 1)

	res, err := DecodeWAV(path, DecodeConfig{})
	if err != nil {
		t.Fatalf("silence should not be a hard error, got: %v", err)
	}
	if res.Text != "" || res.Confidence != 0 {
		t.Fatalf("expected empty zero-confidence result, got %+v", res)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning for the silent recording")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := DecodeWAV(filepath.Join(t.TempDir(), "nope.wav"), DecodeConfig{}); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestNormalizeSamples(t *testing.T) {
	if got := normalizeSamples([]int{0, 255, 128}, 8); math.Abs(got[0]+1) > 1e-9 || math.Abs(got[1]-0.9921875) > 1e-9 || math.Abs(got[2]) > 1e-9 {
		t.Fatalf("8-bit normalization wrong: %v", got)
	}
	if got := normalizeSamples([]int{-32768, 32767}, 16); math.Abs(got[0]+1) > 1e-6 || math.Abs(got[1]-0.999969) > 1e-4 {
		t.Fatalf("16-bit normalization wrong: %v", got)
	}
}
