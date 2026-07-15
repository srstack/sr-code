package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkchannel "github.com/larksuite/oapi-sdk-go/v3/channel"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// larkAPI is the slice of the Lark Open API the hub uses; a fake stands in
// for it in tests.
type larkAPI interface {
	// SendCard posts a card as a chat message (the thread root) and returns
	// its message id.
	SendCard(ctx context.Context, chatID, cardJSON string) (string, error)
	// UpdateCard replaces a sent card's content in place.
	UpdateCard(ctx context.Context, messageID, cardJSON string) error
	// ReplyText replies to rootID inside its thread and returns the thread id
	// (may be "" when the server omits it).
	ReplyText(ctx context.Context, rootID, text string) (string, error)
	// ReplyCard replies with an interactive card inside rootID's thread.
	ReplyCard(ctx context.Context, rootID, cardJSON string) (string, error)
	// ReplyPost replies with a rich-text (post) message inside rootID's thread.
	ReplyPost(ctx context.Context, rootID, postJSON string) (string, error)
	// UploadImage uploads a message image and returns its image_key.
	UploadImage(ctx context.Context, data []byte) (string, error)
	// ReplyImage replies with an uploaded image inside rootID's thread.
	ReplyImage(ctx context.Context, rootID, imageKey string) (string, error)
	// React adds an emoji reaction to a message.
	React(ctx context.Context, messageID, emojiType string) error
	DownloadResource(ctx context.Context, messageID, key, kind string) (io.Reader, string, error)
	ThreadMessages(ctx context.Context, threadID string, afterMs int64, limit int) ([]pulledMsg, bool, error)
	MergedMessages(ctx context.Context, messageID string) ([]pulledMsg, error)
	ChatMemberNames(ctx context.Context, chatID string) (map[string]string, error)
	AppName(ctx context.Context, appID string) (string, error)
	BotInfo(ctx context.Context) (string, error)
}

type mentionRef struct {
	Key    string
	OpenID string
	Name   string
}

// pulledMsg is the hub-facing shape of one thread-history message.
type pulledMsg struct {
	ID          string
	CreateTime  int64
	SenderOpen  string
	SenderAppID string
	MsgType     string
	Content     string
	Mentions    []mentionRef
}

// larkClient implements larkAPI on the official SDK client.
type larkClient struct {
	c *lark.Client
}

func newLarkClient(appID, appSecret, domain string) *larkClient {
	return &larkClient{c: lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(domain))}
}

// textContent renders the im "text" message content JSON.
func textContent(text string) string {
	data, _ := json.Marshal(map[string]string{"text": text})
	return string(data)
}

func (l *larkClient) SendCard(ctx context.Context, chatID, cardJSON string) (string, error) {
	resp, err := l.c.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("send card", resp.Code, resp.Msg)
	}
	return deref(resp.Data.MessageId), nil
}

func (l *larkClient) UpdateCard(ctx context.Context, messageID, cardJSON string) error {
	resp, err := l.c.Im.V1.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return apiErr("update card", resp.Code, resp.Msg)
	}
	return nil
}

// reply sends one threaded reply of any msg type and returns the thread id.
func (l *larkClient) reply(ctx context.Context, rootID, msgType, content string) (string, error) {
	resp, err := l.c.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(rootID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			ReplyInThread(true).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("reply", resp.Code, resp.Msg)
	}
	return deref(resp.Data.ThreadId), nil
}

func (l *larkClient) ReplyText(ctx context.Context, rootID, text string) (string, error) {
	return l.reply(ctx, rootID, larkim.MsgTypeText, textContent(text))
}

func (l *larkClient) ReplyCard(ctx context.Context, rootID, cardJSON string) (string, error) {
	return l.reply(ctx, rootID, larkim.MsgTypeInteractive, cardJSON)
}

func (l *larkClient) ReplyPost(ctx context.Context, rootID, postJSON string) (string, error) {
	return l.reply(ctx, rootID, larkim.MsgTypePost, postJSON)
}

func (l *larkClient) UploadImage(ctx context.Context, data []byte) (string, error) {
	resp, err := l.c.Im.V1.Image.Create(ctx, larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(larkim.CreateImageImageTypeMessage).
			Image(bytes.NewReader(data)).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("upload image", resp.Code, resp.Msg)
	}
	return deref(resp.Data.ImageKey), nil
}

func (l *larkClient) ReplyImage(ctx context.Context, rootID, imageKey string) (string, error) {
	data, _ := json.Marshal(map[string]string{"image_key": imageKey})
	return l.reply(ctx, rootID, larkim.MsgTypeImage, string(data))
}

func (l *larkClient) React(ctx context.Context, messageID, emojiType string) error {
	resp, err := l.c.Im.V1.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return apiErr("react", resp.Code, resp.Msg)
	}
	return nil
}

func (l *larkClient) DownloadResource(ctx context.Context, messageID, key, kind string) (io.Reader, string, error) {
	resp, err := l.c.Im.V1.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(key).
		Type(kind).
		Build())
	if err != nil {
		return nil, "", err
	}
	if !resp.Success() {
		return nil, "", apiErr("get message resource", resp.Code, resp.Msg)
	}
	return resp.File, resp.FileName, nil
}

func (l *larkClient) ThreadMessages(ctx context.Context, threadID string, afterMs int64, limit int) ([]pulledMsg, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	builder := larkim.NewListMessageReqBuilder().
		ContainerIdType("thread").
		ContainerId(threadID).
		SortType(larkim.ReadHistoryMessageV1SortTypeByCreateTimeAsc).
		PageSize(50)
	if afterMs > 0 {
		builder.StartTime(strconv.FormatInt(afterMs/1000, 10))
	}
	var out []pulledMsg
	truncated := false
	pageToken := ""
	for {
		req := builder
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}
		resp, err := l.c.Im.V1.Message.List(ctx, req.Build())
		if err != nil {
			return nil, false, err
		}
		if !resp.Success() {
			return nil, false, apiErr("list messages", resp.Code, resp.Msg)
		}
		if resp.Data != nil {
			for _, m := range resp.Data.Items {
				pm := convertPulled(m)
				// < not <=: same-ms siblings of the watermark must reach
				// the hub's WMID tie-break.
				if pm.ID == "" || pm.CreateTime < afterMs {
					continue
				}
				out = append(out, pm)
				if len(out) > limit {
					truncated = true
					out = out[len(out)-limit:]
				}
			}
			if resp.Data.HasMore == nil || !*resp.Data.HasMore {
				break
			}
			pageToken = deref(resp.Data.PageToken)
			if pageToken == "" {
				break
			}
		} else {
			break
		}
	}
	return out, truncated, nil
}

func (l *larkClient) MergedMessages(ctx context.Context, messageID string) ([]pulledMsg, error) {
	resp, err := l.c.Im.V1.Message.Get(ctx, larkim.NewGetMessageReqBuilder().
		MessageId(messageID).
		Build())
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, apiErr("get merged message", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return nil, nil
	}
	out := make([]pulledMsg, 0, len(resp.Data.Items))
	for _, item := range resp.Data.Items {
		if item == nil || deref(item.UpperMessageId) == "" {
			continue
		}
		out = append(out, convertPulled(item))
	}
	return out, nil
}

func convertPulled(m *larkim.Message) pulledMsg {
	if m == nil {
		return pulledMsg{}
	}
	create, _ := strconv.ParseInt(deref(m.CreateTime), 10, 64)
	pm := pulledMsg{
		ID:         deref(m.MessageId),
		CreateTime: create,
		MsgType:    deref(m.MsgType),
	}
	if m.Body != nil {
		pm.Content = deref(m.Body.Content)
	}
	if m.Sender != nil {
		if deref(m.Sender.SenderType) == "app" {
			pm.SenderAppID = deref(m.Sender.Id)
		} else {
			pm.SenderOpen = deref(m.Sender.Id)
		}
	}
	for _, mr := range m.Mentions {
		if mr == nil {
			continue
		}
		pm.Mentions = append(pm.Mentions, mentionRef{
			Key:    deref(mr.Key),
			OpenID: deref(mr.Id),
			Name:   deref(mr.Name),
		})
	}
	return pm
}

func (l *larkClient) ChatMemberNames(ctx context.Context, chatID string) (map[string]string, error) {
	names := map[string]string{}
	pageToken := ""
	for {
		req := larkim.NewGetChatMembersReqBuilder().
			ChatId(chatID).
			MemberIdType("open_id").
			PageSize(100)
		if pageToken != "" {
			req.PageToken(pageToken)
		}
		resp, err := l.c.Im.V1.ChatMembers.Get(ctx, req.Build())
		if err != nil {
			return map[string]string{}, nil
		}
		if !resp.Success() {
			return map[string]string{}, nil
		}
		if resp.Data != nil {
			for _, m := range resp.Data.Items {
				if m == nil {
					continue
				}
				id, name := deref(m.MemberId), deref(m.Name)
				if id != "" && name != "" {
					names[id] = name
				}
			}
			if resp.Data.HasMore == nil || !*resp.Data.HasMore {
				break
			}
			pageToken = deref(resp.Data.PageToken)
			if pageToken == "" {
				break
			}
		} else {
			break
		}
	}
	return names, nil
}

func (l *larkClient) AppName(ctx context.Context, appID string) (string, error) {
	resp, err := l.c.Application.V6.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().
		AppId(appID).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("get application", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.App == nil {
		return "", errors.New("lark get application: empty response")
	}
	return strings.TrimSpace(deref(resp.Data.App.AppName)), nil
}

func (l *larkClient) BotInfo(ctx context.Context) (string, error) {
	identity := larkchannel.NewChannel(l.c, nil).GetBotIdentity(ctx)
	if identity == nil || identity.OpenID == "" {
		return "", fmt.Errorf("empty bot open_id")
	}
	return identity.OpenID, nil
}

func apiErr(op string, code int, msg string) error {
	return fmt.Errorf("lark %s: code %d: %s", op, code, msg)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
