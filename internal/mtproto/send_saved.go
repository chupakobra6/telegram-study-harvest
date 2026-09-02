package mtproto

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chupakobra6/telegram-harvest/internal/config"
	"github.com/chupakobra6/telegram-harvest/internal/harvest"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/tg"
)

const SavedMessagesMainUsername = "Pheik13"

type SendSavedOptions struct {
	Text     string
	FilePath string
	Caption  string
}

type SendSavedResult struct {
	Destination string                `json:"destination"`
	Profile     harvest.SelfProfile   `json:"profile"`
	Message     harvest.MessageRecord `json:"message"`
	Verified    bool                  `json:"verified"`
}

type savedFileMetadata struct {
	Path     string
	Name     string
	MIMEType string
	Size     int64
}

// SendSaved is the sole Telegram write primitive exposed by telegram-harvest.
// It is intentionally bound to the main profile and InputPeerSelf: callers
// cannot provide or resolve a recipient.
func (c *Client) SendSaved(ctx context.Context, opts SendSavedOptions) (SendSavedResult, error) {
	if c.cfg.Mode != config.ModeMain {
		return SendSavedResult{}, fmt.Errorf("send-saved is supported only for profile main")
	}

	normalized, fileMetadata, err := normalizeSendSavedOptions(opts)
	if err != nil {
		return SendSavedResult{}, err
	}

	var result SendSavedResult
	err = c.RunAuthorized(ctx, func(runCtx context.Context, session *Session) error {
		var sendErr error
		result, sendErr = session.sendSaved(runCtx, normalized, fileMetadata)
		return sendErr
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func normalizeSendSavedOptions(opts SendSavedOptions) (SendSavedOptions, *savedFileMetadata, error) {
	hasText := strings.TrimSpace(opts.Text) != ""
	hasFile := strings.TrimSpace(opts.FilePath) != ""
	if hasText == hasFile {
		return SendSavedOptions{}, nil, fmt.Errorf("provide exactly one of --text or --file")
	}
	if !hasFile {
		if strings.TrimSpace(opts.Caption) != "" {
			return SendSavedOptions{}, nil, fmt.Errorf("--caption requires --file")
		}
		return opts, nil, nil
	}

	absolutePath, err := filepath.Abs(strings.TrimSpace(opts.FilePath))
	if err != nil {
		return SendSavedOptions{}, nil, fmt.Errorf("resolve --file: %w", err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return SendSavedOptions{}, nil, fmt.Errorf("inspect --file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SendSavedOptions{}, nil, fmt.Errorf("--file must be a regular file")
	}

	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name())))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	normalized := opts
	normalized.FilePath = absolutePath
	return normalized, &savedFileMetadata{
		Path:     absolutePath,
		Name:     info.Name(),
		MIMEType: mimeType,
		Size:     info.Size(),
	}, nil
}

func (s *Session) sendSaved(ctx context.Context, opts SendSavedOptions, fileMetadata *savedFileMetadata) (SendSavedResult, error) {
	profile, err := s.SelfProfile(ctx)
	if err != nil {
		return SendSavedResult{}, fmt.Errorf("verify sending account: %w", err)
	}
	if err := validateSavedMessagesProfile(profile); err != nil {
		return SendSavedResult{}, err
	}

	builder := message.NewSender(s.raw).Self()
	var updates tg.UpdatesClass
	if fileMetadata == nil {
		updates, err = builder.Text(ctx, opts.Text)
	} else {
		uploaded, uploadErr := builder.Upload(message.FromPath(fileMetadata.Path)).AsInputFile(ctx)
		if uploadErr != nil {
			return SendSavedResult{}, fmt.Errorf("upload file to Saved Messages: %w", uploadErr)
		}
		document := message.File(uploaded).
			Filename(fileMetadata.Name).
			MIME(fileMetadata.MIMEType)
		if strings.TrimSpace(opts.Caption) != "" {
			document = message.File(uploaded, styling.Plain(opts.Caption)).
				Filename(fileMetadata.Name).
				MIME(fileMetadata.MIMEType)
		}
		updates, err = builder.Media(ctx, document)
	}
	if err != nil {
		return SendSavedResult{}, fmt.Errorf("send to Saved Messages: %w", err)
	}

	messageID, err := unpack.MessageID(updates, nil)
	if err != nil {
		return SendSavedResult{}, fmt.Errorf("message was sent to Saved Messages but its id could not be read: %w", err)
	}
	publicProfile := profile
	publicProfile.Phone = ""
	result := SendSavedResult{
		Destination: "saved_messages",
		Profile:     publicProfile,
	}

	target := resolvedTarget{
		Raw: "me",
		Chat: harvest.Chat{
			ID:       profile.ID,
			Type:     "user",
			Title:    profile.Display,
			Username: profile.Username,
			Display:  "Saved Messages",
		},
		InputPeer: &tg.InputPeerSelf{},
	}
	record, err := s.readBackSavedMessage(ctx, target, messageID)
	if err != nil {
		return result, fmt.Errorf("message %d was sent to Saved Messages but readback failed; do not retry blindly: %w", messageID, err)
	}
	result.Message = record
	if err := verifySavedMessage(record, profile, opts, fileMetadata); err != nil {
		return result, fmt.Errorf("message %d was sent to Saved Messages but verification failed; do not retry blindly: %w", messageID, err)
	}
	result.Verified = true
	return result, nil
}

func validateSavedMessagesProfile(profile harvest.SelfProfile) error {
	if strings.EqualFold(strings.TrimSpace(profile.Username), SavedMessagesMainUsername) {
		return nil
	}
	actual := strings.TrimSpace(profile.Username)
	if actual == "" {
		actual = "<no username>"
	} else {
		actual = "@" + actual
	}
	return fmt.Errorf("profile main is authorized as %s; refusing send because @%s is required", actual, SavedMessagesMainUsername)
}

func (s *Session) readBackSavedMessage(ctx context.Context, target resolvedTarget, messageID int) (harvest.MessageRecord, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		msgClass, entities, err := s.fetchMessageByID(ctx, target, messageID)
		if err == nil {
			record, ok := normalizeRecord(msgClass, target.Chat, entities)
			if !ok {
				return harvest.MessageRecord{}, fmt.Errorf("unsupported message type %T", msgClass)
			}
			return record, nil
		}
		lastErr = err
		if attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return harvest.MessageRecord{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return harvest.MessageRecord{}, lastErr
}

func verifySavedMessage(record harvest.MessageRecord, profile harvest.SelfProfile, opts SendSavedOptions, fileMetadata *savedFileMetadata) error {
	// Telegram represents a message in the user's own Saved Messages without
	// the ordinary outgoing flag. The strong destination proof is that this
	// exact send result was fetched back through InputPeerSelf under the
	// already verified @Pheik13 session.
	if record.Chat.ID != profile.ID || !strings.EqualFold(record.Chat.Username, profile.Username) {
		return fmt.Errorf("readback is not from the authorized account's self chat")
	}
	if fileMetadata == nil {
		if record.Text != strings.TrimSpace(opts.Text) {
			return fmt.Errorf("text readback mismatch")
		}
		return nil
	}
	if record.Text != strings.TrimSpace(opts.Caption) {
		return fmt.Errorf("caption readback mismatch")
	}
	if len(record.Attachments) != 1 {
		return fmt.Errorf("expected one attachment, got %d", len(record.Attachments))
	}
	attachment := record.Attachments[0]
	if attachment.FileName != fileMetadata.Name {
		return fmt.Errorf("filename readback mismatch: got %q, want %q", attachment.FileName, fileMetadata.Name)
	}
	if attachment.MIMEType != fileMetadata.MIMEType {
		return fmt.Errorf("MIME type readback mismatch: got %q, want %q", attachment.MIMEType, fileMetadata.MIMEType)
	}
	if attachment.Size != fileMetadata.Size {
		return fmt.Errorf("file size readback mismatch: got %d, want %d", attachment.Size, fileMetadata.Size)
	}
	return nil
}
