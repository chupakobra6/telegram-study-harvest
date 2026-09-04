package transcribe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chupakobra6/telegram-harvest/internal/stages"
)

const (
	BackendWhisperCPP                = "whispercpp"
	AcceleratorMetal                 = "metal"
	ProductionLanguage               = "auto"
	ProductionThreads                = 4
	ProductionModelFile              = "ggml-large-v3-turbo-q5_0.bin"
	ProductionSpeechGateFile         = "ggml-silero-v6.2.0.bin"
	whisperShortFormStrategy         = "auto-language-no-timestamps-v2"
	whisperLongFormStrategy          = "native-timestamped-v3"
	whisperAdaptiveRoutingPolicy     = "duration-or-leading-silence-v1"
	whisperAdaptiveLanguagePolicy    = "auto-short-detect-russian-punctuation-long-v2"
	whisperRepetitionPolicy          = "exact-token-cycle-v1"
	adaptiveLongMediaSeconds         = 180
	adaptiveLeadingSilenceSeconds    = 10
	adaptiveRepetitionMinRepeats     = 5
	adaptiveRepetitionMinSpanTokens  = 20
	adaptiveRepetitionMaxBlockTokens = 32
	longFormLanguageProbeSeconds     = 15
	longFormRussianInitialPrompt     = "Да. Нет? Хорошо! Пожалуйста, продолжайте."
	longFormCoverageToleranceSeconds = 2.0
	StrategyShortMessage             = whisperShortFormStrategy
	StrategyLongForm                 = whisperLongFormStrategy
)

// Descriptor is the stable identity of the production ASR implementation.
// It is used in timing reports and transcript cache keys, so fields must
// describe everything that can materially change a transcript.
type Descriptor struct {
	Backend      string                       `json:"backend"`
	Accelerator  string                       `json:"accelerator"`
	Model        string                       `json:"model"`
	Language     string                       `json:"language,omitempty"`
	Quantization string                       `json:"quantization,omitempty"`
	Threads      int                          `json:"threads,omitempty"`
	Decode       *WhisperDecodeDescriptor     `json:"decode,omitempty"`
	SpeechGate   *WhisperSpeechGateDescriptor `json:"speech_gate,omitempty"`
	Adaptive     *WhisperAdaptiveDescriptor   `json:"adaptive,omitempty"`
	PostFilter   string                       `json:"post_filter,omitempty"`
}

// WhisperDecodeOptions is intentionally retained as a benchmark-level
// contract. Daily and dump always use ProductionWhisperDecode.
type WhisperDecodeOptions struct {
	BeamSize                *int     `json:"beam_size,omitempty"`
	BestOf                  *int     `json:"best_of,omitempty"`
	Temperature             *float64 `json:"temperature,omitempty"`
	TemperatureIncrement    *float64 `json:"temperature_increment,omitempty"`
	NoSpeechThreshold       *float64 `json:"no_speech_threshold,omitempty"`
	LogProbabilityThreshold *float64 `json:"log_probability_threshold,omitempty"`
	EntropyThreshold        *float64 `json:"entropy_threshold,omitempty"`
	SuppressNonSpeechTokens bool     `json:"suppress_non_speech_tokens,omitempty"`
}

type WhisperDecodeDescriptor struct {
	BeamSize                int     `json:"beam_size"`
	BestOf                  int     `json:"best_of"`
	Temperature             float64 `json:"temperature"`
	TemperatureIncrement    float64 `json:"temperature_increment"`
	NoSpeechThreshold       float64 `json:"no_speech_threshold"`
	LogProbabilityThreshold float64 `json:"log_probability_threshold"`
	EntropyThreshold        float64 `json:"entropy_threshold"`
	SuppressNonSpeechTokens bool    `json:"suppress_non_speech_tokens"`
}

// WhisperSpeechGateOptions is the single Silero configuration. Short media use
// one whole-file bounds scan; long media reuse it in bounded boundary windows.
// Silero never cuts or concatenates speech by itself.
type WhisperSpeechGateOptions struct {
	Enabled              bool     `json:"enabled,omitempty"`
	Command              string   `json:"command,omitempty"`
	ModelPath            string   `json:"model_path,omitempty"`
	Threshold            *float64 `json:"threshold,omitempty"`
	MinSpeechDurationMS  *int     `json:"min_speech_duration_ms,omitempty"`
	MinSilenceDurationMS *int     `json:"min_silence_duration_ms,omitempty"`
	SpeechPadMS          *int     `json:"speech_pad_ms,omitempty"`
}

type WhisperSpeechGateDescriptor struct {
	Enabled              bool    `json:"enabled"`
	Model                string  `json:"model"`
	Threshold            float64 `json:"threshold"`
	MinSpeechDurationMS  int     `json:"min_speech_duration_ms"`
	MinSilenceDurationMS int     `json:"min_silence_duration_ms"`
	SpeechPadMS          int     `json:"speech_pad_ms"`
}

// WhisperAdaptiveOptions keeps one production profile while selecting between
// the exact short-message decode and protected timestamped long-form decode
// after the WAV duration and Silero speech bounds are known.
type WhisperAdaptiveOptions struct {
	Enabled                          bool
	LongMediaSeconds                 float64
	LeadingSilenceSeconds            float64
	ScanWindowSeconds                int
	ScanOverlapSeconds               int
	LeadInMS                         int
	LanguageProbeSeconds             int
	RussianInitialPrompt             string
	CarryInitialPrompt               bool
	TrailingCoverageToleranceSeconds float64
	RepetitionMinRepeats             int
	RepetitionMinSpanTokens          int
	RepetitionMaxBlockTokens         int
}

type WhisperAdaptiveDescriptor struct {
	Enabled                          bool    `json:"enabled"`
	RoutingPolicy                    string  `json:"routing_policy"`
	LongMediaSeconds                 float64 `json:"long_media_seconds"`
	LeadingSilenceSeconds            float64 `json:"leading_silence_seconds"`
	ScanWindowSeconds                int     `json:"scan_window_seconds"`
	ScanOverlapSeconds               int     `json:"scan_overlap_seconds"`
	LeadInMS                         int     `json:"lead_in_ms"`
	LanguagePolicy                   string  `json:"language_policy"`
	LanguageProbeSeconds             int     `json:"language_probe_seconds"`
	RussianInitialPrompt             string  `json:"russian_initial_prompt"`
	CarryInitialPrompt               bool    `json:"carry_initial_prompt"`
	TrailingCoverageToleranceSeconds float64 `json:"trailing_coverage_tolerance_seconds"`
	ShortDecodeStrategy              string  `json:"short_decode_strategy"`
	LongDecodeStrategy               string  `json:"long_decode_strategy"`
	RepetitionPolicy                 string  `json:"repetition_policy"`
	RepetitionMinRepeats             int     `json:"repetition_min_repeats"`
	RepetitionMinSpanTokens          int     `json:"repetition_min_span_tokens"`
	RepetitionMaxBlockTokens         int     `json:"repetition_max_block_tokens"`
}

// Diagnostics record backend confidence and routing evidence. Only explicit
// long-form invariants (timestamps, tail coverage, extreme exact loops) reject
// a result; probability fields remain benchmark signals.
type Diagnostics struct {
	Segments                         int      `json:"segments"`
	MeanAverageLogProb               float64  `json:"mean_average_log_probability,omitempty"`
	MinimumAverageLogProb            float64  `json:"minimum_average_log_probability,omitempty"`
	MeanNoSpeechProb                 float64  `json:"mean_no_speech_probability,omitempty"`
	MaximumNoSpeechProb              float64  `json:"maximum_no_speech_probability,omitempty"`
	SpeechGatePassed                 *bool    `json:"speech_gate_passed,omitempty"`
	LeadingSpeechOffsetSeconds       float64  `json:"leading_speech_offset_seconds,omitempty"`
	TimestampedSegments              bool     `json:"timestamped_segments,omitempty"`
	DecodedAudioDurationSeconds      float64  `json:"decoded_audio_duration_seconds,omitempty"`
	FirstSegmentStartSeconds         float64  `json:"first_segment_start_seconds,omitempty"`
	LastSegmentEndSeconds            float64  `json:"last_segment_end_seconds,omitempty"`
	LastDetectedSpeechEndSeconds     float64  `json:"last_detected_speech_end_seconds,omitempty"`
	TrailingSpeechCoverageGapSeconds float64  `json:"trailing_speech_coverage_gap_seconds"`
	TrailingCoverageToleranceSeconds float64  `json:"trailing_coverage_tolerance_seconds,omitempty"`
	CoverageValidated                bool     `json:"coverage_validated,omitempty"`
	DetectedLanguage                 string   `json:"detected_language,omitempty"`
	LanguageDetectionSeconds         float64  `json:"language_detection_seconds,omitempty"`
	InitialPromptApplied             bool     `json:"initial_prompt_applied"`
	Strategy                         string   `json:"strategy,omitempty"`
	RouteReason                      string   `json:"route_reason,omitempty"`
	SourceAudioDurationSeconds       float64  `json:"source_audio_duration_seconds,omitempty"`
	RepetitionValidated              bool     `json:"repetition_validated,omitempty"`
	ExtremeRepetitionDetected        bool     `json:"extreme_repetition_detected,omitempty"`
	MaximumRepeatedTokenBlock        int      `json:"maximum_repeated_token_block,omitempty"`
	MaximumTokenBlockRepetitions     int      `json:"maximum_token_block_repetitions,omitempty"`
	MaximumRepeatedTokenSpan         int      `json:"maximum_repeated_token_span,omitempty"`
	RemovedTerminalHallucinations    []string `json:"removed_terminal_hallucinations,omitempty"`
}

// ProductionOptions is the only ASR profile used by product commands. Paths
// remain configurable because the runtime is a local dependency; inference
// behavior is deliberately code-owned and regression-tested.
func ProductionOptions(command, modelPath, gateModelPath, ffmpegCommand string, timing stages.Observer) Options {
	return Options{
		WhisperCommand:    command,
		WhisperModelPath:  modelPath,
		WhisperThreads:    ProductionThreads,
		WhisperDecode:     ProductionWhisperDecode(),
		WhisperSpeechGate: ProductionWhisperSpeechGate(gateModelPath),
		WhisperAdaptive:   ProductionWhisperAdaptive(),
		Language:          ProductionLanguage,
		FFmpegCommand:     ffmpegCommand,
		StageTiming:       timing,
		productionProfile: true,
	}
}

func ProductionWhisperAdaptive() WhisperAdaptiveOptions {
	return WhisperAdaptiveOptions{
		Enabled:                          true,
		LongMediaSeconds:                 adaptiveLongMediaSeconds,
		LeadingSilenceSeconds:            adaptiveLeadingSilenceSeconds,
		ScanWindowSeconds:                300,
		ScanOverlapSeconds:               10,
		LeadInMS:                         1000,
		LanguageProbeSeconds:             longFormLanguageProbeSeconds,
		RussianInitialPrompt:             longFormRussianInitialPrompt,
		CarryInitialPrompt:               false,
		TrailingCoverageToleranceSeconds: longFormCoverageToleranceSeconds,
		RepetitionMinRepeats:             adaptiveRepetitionMinRepeats,
		RepetitionMinSpanTokens:          adaptiveRepetitionMinSpanTokens,
		RepetitionMaxBlockTokens:         adaptiveRepetitionMaxBlockTokens,
	}
}

func (o Options) Descriptor() Descriptor {
	decode := o.WhisperDecode.normalized()
	descriptor := Descriptor{
		Backend:      BackendWhisperCPP,
		Accelerator:  AcceleratorMetal,
		Model:        stableModelIdentity(o.WhisperModelPath),
		Language:     normalizedLanguage(o.Language),
		Quantization: whisperQuantization(o.WhisperModelPath),
		Threads:      normalizedThreads(o.WhisperThreads),
		Decode:       &decode,
		PostFilter:   whisperTerminalHallucinationProfile,
	}
	if o.WhisperSpeechGate.Enabled {
		gate := o.WhisperSpeechGate.normalized()
		descriptor.SpeechGate = &gate
	}
	if o.WhisperAdaptive.Enabled {
		adaptive := o.WhisperAdaptive.normalized()
		descriptor.Adaptive = &adaptive
	}
	return descriptor
}

// CacheIdentity v3 intentionally invalidates the earlier split-profile cache:
// routing thresholds can change which decode strategy owns the same media.
// The empty component remains reserved for the removed integrated-VAD slot.
func (o Options) CacheIdentity() string {
	descriptorJSON, _ := json.Marshal(o.Descriptor())
	gateCommand := ""
	gateModel := ""
	if o.WhisperSpeechGate.Enabled {
		gateCommand = cacheModelIdentity(o.WhisperSpeechGate.command(o))
		gateModel = cacheModelIdentity(o.WhisperSpeechGate.ModelPath)
	}
	parts := []string{
		"v3",
		string(descriptorJSON),
		cacheModelIdentity(o.WhisperCommand),
		cacheModelIdentity(o.WhisperModelPath),
		"",
		gateCommand,
		gateModel,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func (o Options) Validate() error {
	if strings.TrimSpace(o.WhisperCommand) == "" {
		return fmt.Errorf("whisper.cpp server command is empty")
	}
	if strings.TrimSpace(o.WhisperModelPath) == "" {
		return fmt.Errorf("whisper.cpp model path is empty")
	}
	if o.productionProfile && filepath.Base(filepath.Clean(o.WhisperModelPath)) != ProductionModelFile {
		return fmt.Errorf("production whisper.cpp model must be %s", ProductionModelFile)
	}
	if err := o.WhisperDecode.validate(); err != nil {
		return err
	}
	if err := o.WhisperSpeechGate.validate(o); err != nil {
		return err
	}
	if err := o.WhisperAdaptive.validate(o); err != nil {
		return err
	}
	if o.productionProfile && o.WhisperSpeechGate.Enabled && filepath.Base(filepath.Clean(o.WhisperSpeechGate.ModelPath)) != ProductionSpeechGateFile {
		return fmt.Errorf("production whisper speech-gate model must be %s", ProductionSpeechGateFile)
	}
	return nil
}

// ValidateRuntime verifies that the production profile's local dependencies
// are present before a caller starts an expensive media pipeline.
func (o Options) ValidateRuntime() error {
	if err := o.Validate(); err != nil {
		return err
	}
	commands := map[string]string{
		"ffmpeg":             o.FFmpegCommand,
		"whisper.cpp server": o.WhisperCommand,
	}
	if o.WhisperSpeechGate.Enabled {
		commands["whisper.cpp speech gate"] = o.WhisperSpeechGate.command(o)
	}
	for name, command := range commands {
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("%s command is empty", name)
		}
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("%s command is unavailable: %s", name, command)
		}
	}
	models := map[string]string{"whisper.cpp model": o.WhisperModelPath}
	if o.WhisperSpeechGate.Enabled {
		models["whisper.cpp speech gate"] = o.WhisperSpeechGate.ModelPath
	}
	for name, path := range models {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s is unavailable: %s: %w", name, path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file: %s", name, path)
		}
	}
	return nil
}

func (o WhisperDecodeOptions) normalized() WhisperDecodeDescriptor {
	return WhisperDecodeDescriptor{
		BeamSize:                intValue(o.BeamSize, -1),
		BestOf:                  intValue(o.BestOf, 2),
		Temperature:             floatValue(o.Temperature, 0),
		TemperatureIncrement:    floatValue(o.TemperatureIncrement, 0.2),
		NoSpeechThreshold:       floatValue(o.NoSpeechThreshold, 0.6),
		LogProbabilityThreshold: floatValue(o.LogProbabilityThreshold, -1),
		EntropyThreshold:        floatValue(o.EntropyThreshold, 2.4),
		SuppressNonSpeechTokens: o.SuppressNonSpeechTokens,
	}
}

func ProductionWhisperDecode() WhisperDecodeOptions {
	beamSize := 5
	return WhisperDecodeOptions{BeamSize: &beamSize}
}

func ProductionWhisperSpeechGate(modelPath string) WhisperSpeechGateOptions {
	threshold := 0.5
	minSpeechDurationMS := 250
	minSilenceDurationMS := 100
	speechPadMS := 30
	return WhisperSpeechGateOptions{
		Enabled:              true,
		ModelPath:            modelPath,
		Threshold:            &threshold,
		MinSpeechDurationMS:  &minSpeechDurationMS,
		MinSilenceDurationMS: &minSilenceDurationMS,
		SpeechPadMS:          &speechPadMS,
	}
}

func (o WhisperDecodeOptions) validate() error {
	value := o.normalized()
	if value.BeamSize == 0 || value.BeamSize < -1 {
		return fmt.Errorf("whisper beam size must be -1 or positive")
	}
	if value.BestOf < 1 {
		return fmt.Errorf("whisper best-of must be positive")
	}
	if value.Temperature < 0 || value.TemperatureIncrement < 0 {
		return fmt.Errorf("whisper temperatures must be non-negative")
	}
	if value.NoSpeechThreshold < 0 || value.NoSpeechThreshold > 1 {
		return fmt.Errorf("whisper no-speech threshold must be between 0 and 1")
	}
	return nil
}

func (o WhisperSpeechGateOptions) normalized() WhisperSpeechGateDescriptor {
	return WhisperSpeechGateDescriptor{
		Enabled:              o.Enabled,
		Model:                stableModelIdentity(o.ModelPath),
		Threshold:            floatValue(o.Threshold, 0.5),
		MinSpeechDurationMS:  intValue(o.MinSpeechDurationMS, 250),
		MinSilenceDurationMS: intValue(o.MinSilenceDurationMS, 100),
		SpeechPadMS:          intValue(o.SpeechPadMS, 30),
	}
}

func (o WhisperSpeechGateOptions) validate(parent Options) error {
	if !o.Enabled {
		return nil
	}
	if strings.TrimSpace(o.command(parent)) == "" {
		return fmt.Errorf("whisper speech-gate command is empty")
	}
	if strings.TrimSpace(o.ModelPath) == "" {
		return fmt.Errorf("whisper speech-gate model is empty")
	}
	value := o.normalized()
	if value.Threshold < 0 || value.Threshold > 1 {
		return fmt.Errorf("whisper speech-gate threshold must be between 0 and 1")
	}
	if value.MinSpeechDurationMS < 0 || value.MinSilenceDurationMS < 0 || value.SpeechPadMS < 0 {
		return fmt.Errorf("whisper speech-gate durations must be non-negative")
	}
	return nil
}

func (o WhisperSpeechGateOptions) command(parent Options) string {
	if value := strings.TrimSpace(o.Command); value != "" {
		return value
	}
	server := strings.TrimSpace(parent.WhisperCommand)
	if server == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(server), "whisper-vad-speech-segments")
}

func (o WhisperAdaptiveOptions) normalized() WhisperAdaptiveDescriptor {
	return WhisperAdaptiveDescriptor{
		Enabled:                          o.Enabled,
		RoutingPolicy:                    whisperAdaptiveRoutingPolicy,
		LongMediaSeconds:                 positiveFloatOr(o.LongMediaSeconds, adaptiveLongMediaSeconds),
		LeadingSilenceSeconds:            positiveFloatOr(o.LeadingSilenceSeconds, adaptiveLeadingSilenceSeconds),
		ScanWindowSeconds:                positiveOr(o.ScanWindowSeconds, 300),
		ScanOverlapSeconds:               positiveOr(o.ScanOverlapSeconds, 10),
		LeadInMS:                         positiveOr(o.LeadInMS, 1000),
		LanguagePolicy:                   whisperAdaptiveLanguagePolicy,
		LanguageProbeSeconds:             positiveOr(o.LanguageProbeSeconds, longFormLanguageProbeSeconds),
		RussianInitialPrompt:             strings.TrimSpace(o.RussianInitialPrompt),
		CarryInitialPrompt:               o.CarryInitialPrompt,
		TrailingCoverageToleranceSeconds: positiveFloatOr(o.TrailingCoverageToleranceSeconds, longFormCoverageToleranceSeconds),
		ShortDecodeStrategy:              whisperShortFormStrategy,
		LongDecodeStrategy:               whisperLongFormStrategy,
		RepetitionPolicy:                 whisperRepetitionPolicy,
		RepetitionMinRepeats:             positiveOr(o.RepetitionMinRepeats, adaptiveRepetitionMinRepeats),
		RepetitionMinSpanTokens:          positiveOr(o.RepetitionMinSpanTokens, adaptiveRepetitionMinSpanTokens),
		RepetitionMaxBlockTokens:         positiveOr(o.RepetitionMaxBlockTokens, adaptiveRepetitionMaxBlockTokens),
	}
}

func (o WhisperAdaptiveOptions) validate(parent Options) error {
	if !o.Enabled {
		return nil
	}
	if !parent.WhisperSpeechGate.Enabled {
		return fmt.Errorf("whisper adaptive routing requires the speech gate")
	}
	value := o.normalized()
	if value.LongMediaSeconds <= value.LeadingSilenceSeconds || value.LeadInMS < 0 {
		return fmt.Errorf("whisper adaptive routing thresholds are invalid")
	}
	if value.ScanWindowSeconds <= value.ScanOverlapSeconds {
		return fmt.Errorf("whisper adaptive scan window must exceed overlap")
	}
	if value.LanguageProbeSeconds <= 0 || value.RussianInitialPrompt == "" {
		return fmt.Errorf("whisper adaptive language policy is incomplete")
	}
	if value.TrailingCoverageToleranceSeconds <= 0 {
		return fmt.Errorf("whisper adaptive coverage tolerance must be positive")
	}
	if value.RepetitionMinRepeats < 2 || value.RepetitionMinSpanTokens < value.RepetitionMinRepeats || value.RepetitionMaxBlockTokens < 1 {
		return fmt.Errorf("whisper adaptive repetition policy is invalid")
	}
	return nil
}

func positiveOr(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func positiveFloatOr(value, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

func normalizedThreads(threads int) int {
	if threads > 0 {
		return threads
	}
	return ProductionThreads
}

func intValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func floatValue(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func stableModelIdentity(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(path))
}

func cacheModelIdentity(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	cleaned, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		cleaned = filepath.Clean(path)
	}
	info, statErr := os.Stat(cleaned)
	if statErr != nil {
		return cleaned
	}
	return strings.Join([]string{
		cleaned,
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatInt(info.ModTime().UnixNano(), 10),
	}, ":")
}

func whisperQuantization(path string) string {
	name := strings.ToLower(filepath.Base(path))
	for _, marker := range []string{"q2_k", "q3_k", "q4_0", "q4_1", "q4_k", "q5_0", "q5_1", "q5_k", "q6_k", "q8_0"} {
		if strings.Contains(name, marker) {
			return marker
		}
	}
	return "f16"
}
