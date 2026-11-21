[![GitHub release](https://img.shields.io/badge/release-v1.0.0-brightgreen?style=flat-square)](https://github.com/SaDs3c/morseCode/releases/tag/V1.0.0)
[![GitHub stars](https://img.shields.io/github/stars/SaDs3c/sadExtractor?style=flat-square)](https://github.com/SaDs3c/morseCode/stargazers)
[![GitHub issues](https://img.shields.io/github/issues/SaDs3c/sadExtractor?style=flat-square)](https://github.com/SaDs3c/morseCode/issues)
[![GitHub license](https://img.shields.io/github/license/SaDs3c/sadExtractor?style=flat-square)](https://github.com/SaDs3c/morseCode/blob/main/LICENSE)

# morseCode
morseCode generates Morse code audio from text and decodes mono PCM WAV Morse
recordings back into text with a confidence report.

## Install

If you have Go installed and configured (i.e. with `$GOPATH/bin` in your `$PATH`):

```
go install -v github.com/SaDs3c/morseCode@latest
```

Otherwise [download a release for your platform](https://github.com/SaDs3c/morseCode/releases).
To make it easier to execute you can put the binary in your `$PATH`.

## Usage

Encode text to a Morse code WAV file:

```
morsecode -t "TEXT" [-o output.wav]
```

Decode a mono PCM WAV Morse recording back to text:

```
morsecode -d input.wav [-wpm N] [-json]
```

### Decode flags

| Flag    | Description |
|---------|-------------|
| `-d`    | Path to a mono PCM WAV recording to decode |
| `-wpm`  | Morse speed in words per minute; `0` (default) auto-estimates the dot duration from the signal |
| `-json` | Emit the decoded text and full confidence report as JSON instead of the human-readable report |

### How decoding works

1. The WAV header is validated: mono channel, uncompressed PCM, 8/16/24/32-bit
   samples. Stereo files, non-PCM formats, truncated data chunks and recordings
   longer than 10 minutes are rejected with an explicit error.
2. The signal is split into 10 ms RMS frames. Leading/trailing silence is
   ignored and the on/off threshold adapts to the observed noise floor and tone
   level, so volume changes and light background noise are tolerated.
3. The dot duration (unit) is estimated from the shortest tone bursts, or taken
   from `-wpm` (`unit = 1.2 / wpm` seconds). Bursts classify as dots (`1u`) and
   dashes (`3u`); gaps classify as element (`1u`), letter (`3u`) and word (`7u`)
   separations.
4. Morse groups map back to letters. Bursts or gaps near a classification
   boundary are marked ambiguous, unrecognized sequences become `?`, and both
   lower the reported confidence together with any low-SNR or implausible-timing
   warnings.

### Decode errors and low confidence

- **Hard errors** (non-zero exit): multi-channel or non-PCM WAV, truncated
  files, overlong recordings, missing files, non-finite or negative `-wpm`.
- **Low confidence** (exit zero, text still printed): ambiguous timing, unknown
  Morse sequences, low SNR, or silence-only recordings. Inspect the warnings
  list (or the JSON output) before trusting the text in these cases.
