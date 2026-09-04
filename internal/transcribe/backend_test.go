package transcribe

import (
	"path/filepath"
	"testing"
)

func TestDescriptorAndCacheIdentitySeparateWhisperVariants(t *testing.T) {
	base := Options{
		WhisperCommand:   "/tmp/whisper-server",
		WhisperModelPath: "/models/ggml-small.bin",
		WhisperThreads:   4,
		Language:         "ru",
	}
	quantized := base
	quantized.WhisperModelPath = "/models/ggml-small-q5_0.bin"
	english := base
	english.Language = "en"
	differentBinary := base
	differentBinary.WhisperCommand = "/tmp/whisper-server-next"
	beamSize := 5
	beamSearch := base
	beamSearch.WhisperDecode.BeamSize = &beamSize
	noFallbackIncrement := 0.0
	noFallback := base
	noFallback.WhisperDecode.TemperatureIncrement = &noFallbackIncrement
	speechGate := base
	speechGate.WhisperSpeechGate = WhisperSpeechGateOptions{
		Enabled:   true,
		Command:   "/tmp/whisper-vad-speech-segments",
		ModelPath: "/models/ggml-silero-v6.2.0.bin",
	}
	adaptive := speechGate
	adaptive.WhisperAdaptive = WhisperAdaptiveOptions{Enabled: true, RussianInitialPrompt: "Первый prompt."}
	differentPrompt := adaptive
	differentPrompt.WhisperAdaptive.RussianInitialPrompt = "Второй prompt."
	differentThreshold := adaptive
	differentThreshold.WhisperAdaptive.LongMediaSeconds = 240
	differentRepetitionPolicy := adaptive
	differentRepetitionPolicy.WhisperAdaptive.RepetitionMinRepeats = 7

	identities := map[string]bool{}
	for name, opts := range map[string]Options{
		"base":                        base,
		"quantized":                   quantized,
		"english":                     english,
		"different-binary":            differentBinary,
		"beam-search":                 beamSearch,
		"no-fallback":                 noFallback,
		"speech-gate":                 speechGate,
		"adaptive":                    adaptive,
		"different-prompt":            differentPrompt,
		"different-threshold":         differentThreshold,
		"different-repetition-policy": differentRepetitionPolicy,
	} {
		identity := opts.CacheIdentity()
		if identity == "" {
			t.Fatalf("%s cache identity is empty", name)
		}
		if identities[identity] {
			t.Fatalf("%s cache identity collides: %s", name, identity)
		}
		identities[identity] = true
	}
	if got := quantized.Descriptor().Quantization; got != "q5_0" {
		t.Fatalf("quantization = %q, want q5_0", got)
	}
	if got := beamSearch.Descriptor().Decode.BeamSize; got != 5 {
		t.Fatalf("beam size = %d, want 5", got)
	}
	if got := noFallback.Descriptor().Decode.TemperatureIncrement; got != 0 {
		t.Fatalf("temperature increment = %f, want 0", got)
	}
	if got := speechGate.Descriptor().SpeechGate.Model; got != "ggml-silero-v6.2.0.bin" {
		t.Fatalf("speech gate model = %q", got)
	}
}

func TestProductionWhisperProfileIsPinned(t *testing.T) {
	opts := ProductionOptions(
		"whisper-server",
		"ggml-large-v3-turbo-q5_0.bin",
		"ggml-silero-v6.2.0.bin",
		"ffmpeg",
		nil,
	)
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	descriptor := opts.Descriptor()
	if descriptor.Backend != BackendWhisperCPP || descriptor.Accelerator != AcceleratorMetal {
		t.Fatalf("production backend = %#v", descriptor)
	}
	if descriptor.Language != ProductionLanguage || descriptor.Threads != ProductionThreads {
		t.Fatalf("production language/threads = %q/%d", descriptor.Language, descriptor.Threads)
	}
	if descriptor.Language != "auto" {
		t.Fatalf("production language = %q, want automatic detection", descriptor.Language)
	}
	forcedRussian := opts
	forcedRussian.Language = "ru"
	if opts.CacheIdentity() == forcedRussian.CacheIdentity() {
		t.Fatal("automatic-language production profile reused the forced-Russian cache identity")
	}
	if descriptor.Decode == nil || descriptor.Decode.BeamSize != 5 {
		t.Fatalf("production beam size = %#v, want 5", descriptor.Decode)
	}
	if descriptor.SpeechGate == nil {
		t.Fatal("production speech gate is disabled")
	}
	if descriptor.Adaptive == nil || descriptor.Adaptive.RoutingPolicy != whisperAdaptiveRoutingPolicy {
		t.Fatalf("production adaptive policy = %#v", descriptor.Adaptive)
	}
	if descriptor.SpeechGate.Threshold != 0.5 ||
		descriptor.SpeechGate.MinSpeechDurationMS != 250 ||
		descriptor.SpeechGate.MinSilenceDurationMS != 100 ||
		descriptor.SpeechGate.SpeechPadMS != 30 {
		t.Fatalf("production speech gate = %#v", descriptor.SpeechGate)
	}
	if descriptor.PostFilter != whisperTerminalHallucinationProfile {
		t.Fatalf("production post-filter = %q", descriptor.PostFilter)
	}
}

func TestProductionWhisperProfileUsesOneAdaptivePolicy(t *testing.T) {
	opts := ProductionOptions(
		"/runtime/bin/whisper-server",
		ProductionModelFile,
		ProductionSpeechGateFile,
		"ffmpeg",
		nil,
	)
	if err := opts.Validate(); err != nil {
		t.Fatal(err)
	}
	descriptor := opts.Descriptor()
	if descriptor.SpeechGate == nil || descriptor.SpeechGate.Model != ProductionSpeechGateFile {
		t.Fatalf("speech gate = %#v", descriptor.SpeechGate)
	}
	if descriptor.Adaptive == nil || descriptor.Adaptive.LongMediaSeconds != adaptiveLongMediaSeconds ||
		descriptor.Adaptive.LeadingSilenceSeconds != adaptiveLeadingSilenceSeconds ||
		descriptor.Adaptive.ScanWindowSeconds != 300 || descriptor.Adaptive.ScanOverlapSeconds != 10 ||
		descriptor.Adaptive.LeadInMS != 1000 ||
		descriptor.Adaptive.LanguagePolicy != whisperAdaptiveLanguagePolicy ||
		descriptor.Adaptive.LanguageProbeSeconds != longFormLanguageProbeSeconds ||
		descriptor.Adaptive.RussianInitialPrompt != longFormRussianInitialPrompt ||
		descriptor.Adaptive.CarryInitialPrompt ||
		descriptor.Adaptive.TrailingCoverageToleranceSeconds != longFormCoverageToleranceSeconds ||
		descriptor.Adaptive.ShortDecodeStrategy != whisperShortFormStrategy ||
		descriptor.Adaptive.LongDecodeStrategy != whisperLongFormStrategy ||
		descriptor.Adaptive.RepetitionPolicy != whisperRepetitionPolicy ||
		descriptor.Adaptive.RepetitionMinRepeats != adaptiveRepetitionMinRepeats ||
		descriptor.Adaptive.RepetitionMinSpanTokens != adaptiveRepetitionMinSpanTokens ||
		descriptor.Adaptive.RepetitionMaxBlockTokens != adaptiveRepetitionMaxBlockTokens {
		t.Fatalf("adaptive = %#v", descriptor.Adaptive)
	}
	if descriptor.Language != ProductionLanguage {
		t.Fatalf("default short language = %q, want %q", descriptor.Language, ProductionLanguage)
	}
}

func TestProductionWhisperProfileRejectsDifferentModels(t *testing.T) {
	opts := ProductionOptions(
		"whisper-server",
		"ggml-small-q5_1.bin",
		ProductionSpeechGateFile,
		"ffmpeg",
		nil,
	)
	if err := opts.Validate(); err == nil {
		t.Fatal("production profile accepted a different Whisper model")
	}
	opts = ProductionOptions(
		"whisper-server",
		ProductionModelFile,
		"ggml-silero-old.bin",
		"ffmpeg",
		nil,
	)
	if err := opts.Validate(); err == nil {
		t.Fatal("production profile accepted a different speech-gate model")
	}
}

func TestValidateWhisperConfiguration(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		ok   bool
	}{
		{name: "whisper", opts: Options{WhisperCommand: "server", WhisperModelPath: "model"}, ok: true},
		{name: "missing command", opts: Options{WhisperModelPath: "model"}, ok: false},
		{name: "missing model", opts: Options{WhisperCommand: "server"}, ok: false},
		{name: "bad no speech threshold", opts: whisperWithNoSpeechThreshold(1.1), ok: false},
		{name: "missing gate model", opts: Options{
			WhisperCommand: "server", WhisperModelPath: "model",
			WhisperSpeechGate: WhisperSpeechGateOptions{Enabled: true},
		}, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.opts.Validate()
			if (err == nil) != test.ok {
				t.Fatalf("Validate() error = %v, ok = %t", err, test.ok)
			}
		})
	}
}

func whisperWithNoSpeechThreshold(value float64) Options {
	return Options{
		WhisperCommand:   "server",
		WhisperModelPath: "model",
		WhisperDecode: WhisperDecodeOptions{
			NoSpeechThreshold: &value,
		},
	}
}

func TestStableModelIdentityDoesNotEmbedPrivatePath(t *testing.T) {
	opts := Options{
		WhisperCommand:   "server",
		WhisperModelPath: filepath.Join("/Users/private", "models", "ggml-small.bin"),
	}
	if got := opts.Descriptor().Model; got != "ggml-small.bin" {
		t.Fatalf("model identity = %q", got)
	}
}
