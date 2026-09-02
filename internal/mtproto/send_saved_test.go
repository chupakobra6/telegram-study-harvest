package mtproto

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chupakobra6/telegram-harvest/internal/config"
	"github.com/chupakobra6/telegram-harvest/internal/harvest"
)

func TestSendSavedRejectsStudyProfileBeforeConnecting(t *testing.T) {
	client := New(config.Config{Mode: config.ModeStudy})
	_, err := client.SendSaved(context.Background(), SendSavedOptions{Text: "test"})
	if err == nil || !strings.Contains(err.Error(), "only for profile main") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateSavedMessagesProfileRequiresPheik13(t *testing.T) {
	if err := validateSavedMessagesProfile(harvest.SelfProfile{Username: "pheik13"}); err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"Pheik4", "Pheik15", ""} {
		err := validateSavedMessagesProfile(harvest.SelfProfile{Username: username})
		if err == nil || !strings.Contains(err.Error(), "@Pheik13 is required") {
			t.Fatalf("username=%q err=%v", username, err)
		}
	}
}

func TestNormalizeSendSavedOptionsRequiresExactlyOnePayload(t *testing.T) {
	for _, opts := range []SendSavedOptions{
		{},
		{Text: "hello", FilePath: "/tmp/file.pdf"},
	} {
		if _, _, err := normalizeSendSavedOptions(opts); err == nil {
			t.Fatalf("expected payload validation error for %+v", opts)
		}
	}
	if _, _, err := normalizeSendSavedOptions(SendSavedOptions{Text: "hello", Caption: "caption"}); err == nil {
		t.Fatal("caption without file must fail")
	}
}

func TestNormalizeSendSavedFilePinsFilenameMIMEAndSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Договор.pdf")
	if err := os.WriteFile(path, []byte("pdf-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	normalized, metadata, err := normalizeSendSavedOptions(SendSavedOptions{FilePath: path, Caption: "Подписать"})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.FilePath != path || metadata.Path != path {
		t.Fatalf("paths = %q, %q", normalized.FilePath, metadata.Path)
	}
	if metadata.Name != "Договор.pdf" || metadata.MIMEType != "application/pdf" || metadata.Size != 9 {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestVerifySavedMessageChecksSelfChatAndFileMetadata(t *testing.T) {
	profile := harvest.SelfProfile{ID: 42, Username: SavedMessagesMainUsername}
	opts := SendSavedOptions{FilePath: "/tmp/Договор.pdf", Caption: "Подписать"}
	metadata := &savedFileMetadata{Name: "Договор.pdf", MIMEType: "application/pdf", Size: 123}
	record := harvest.MessageRecord{
		Chat: harvest.Chat{ID: 42, Username: SavedMessagesMainUsername},
		Text: "Подписать",
		Attachments: []harvest.Attachment{{
			FileName: "Договор.pdf",
			MIMEType: "application/pdf",
			Size:     123,
		}},
	}
	if err := verifySavedMessage(record, profile, opts, metadata); err != nil {
		t.Fatal(err)
	}
	if record.Outgoing {
		t.Fatal("fixture must cover Telegram's Saved Messages readback without an outgoing flag")
	}
	record.Chat.ID = 99
	if err := verifySavedMessage(record, profile, opts, metadata); err == nil {
		t.Fatal("different chat must fail verification")
	}
}
