package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// handleAddSource classifies the /addsource argument, then delegates validation
// and persistence to the wired addSource closure (which does a trial collect
// before inserting). All user errors surface as plain Russian text.
func (b *Bot) handleAddSource(ctx context.Context, arg string) {
	if b.addSource == nil {
		b.reply(ctx, "Добавление источников недоступно.")
		return
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		b.reply(ctx, "Использование: /addsource <URL RSS-ленты | @channel>")
		return
	}

	kind, ref, err := b.detectKind(ctx, arg)
	if err != nil {
		b.reply(ctx, "Не удалось распознать источник: "+err.Error())
		return
	}

	id, fresh, err := b.addSource(ctx, kind, ref)
	if err != nil {
		// addSource does validation (trial Collect) + insert; a validation
		// failure is a normal user error, surfaced as plain text.
		b.reply(ctx, "❌ Источник не добавлен: "+err.Error())
		return
	}
	if kind == "tg_botapi" {
		b.reply(ctx, fmt.Sprintf(
			"✅ Источник #%d добавлен (%s: %s). Посты появятся при новых публикациях канала.",
			id, kind, ref))
		return
	}
	b.reply(ctx, fmt.Sprintf("✅ Источник #%d добавлен (%s: %s). Свежих постов: %d.",
		id, kind, ref, fresh))
}

// handleRemoveSource disables a source by numeric id (see /sources).
func (b *Bot) handleRemoveSource(ctx context.Context, arg string) {
	if b.removeSource == nil {
		b.reply(ctx, "Удаление источников недоступно.")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil {
		b.reply(ctx, "Использование: /removesource <числовой id> (см. /sources)")
		return
	}
	found, err := b.removeSource(ctx, id)
	if err != nil {
		b.log.Error("remove source failed", "error", err)
		b.reply(ctx, "❌ Внутренняя ошибка при удалении источника.")
		return
	}
	if !found {
		b.reply(ctx, fmt.Sprintf("Источника с id %d нет. /sources — список.", id))
		return
	}
	b.reply(ctx, fmt.Sprintf("✅ Источник #%d отключён.", id))
}

// detectKind classifies a raw /addsource argument into (kind, ref).
//   - "http://" / "https://" prefix -> ("rss", <url>)
//   - "@channel" (leading '@' required) -> "tg_botapi" if bot is admin there,
//     else "tg_public"
//
// ref is normalized: full URL for rss, channel WITHOUT '@' for tg_*.
func (b *Bot) detectKind(ctx context.Context, arg string) (kind, ref string, err error) {
	arg = strings.TrimSpace(arg)
	low := strings.ToLower(arg)
	switch {
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		return "rss", arg, nil
	case strings.HasPrefix(arg, "@"):
		ch := strings.TrimPrefix(arg, "@")
		if ch == "" {
			return "", "", fmt.Errorf("пустое имя канала")
		}
		admin, aerr := b.isBotAdmin(ctx, ch)
		if aerr != nil {
			// Not admin / channel private to the bot: fall back to public scrape.
			b.log.Info("addsource: not admin, using public scrape", "channel", ch, "err", aerr)
			return "tg_public", ch, nil
		}
		if admin {
			return "tg_botapi", ch, nil
		}
		return "tg_public", ch, nil
	default:
		return "", "", fmt.Errorf("ожидается URL (http/https) или @channel")
	}
}

// isBotAdmin reports whether the bot is an administrator (or owner) of @channel.
// channel is WITHOUT leading '@'. Any API error (channel not found, bot not a
// member) is returned so the caller can fall back to public scraping.
func (b *Bot) isBotAdmin(ctx context.Context, channel string) (bool, error) {
	member, err := b.api.GetChatMember(ctx, &bot.GetChatMemberParams{
		ChatID: "@" + channel,
		UserID: b.api.ID(),
	})
	if err != nil {
		return false, err
	}
	return member.Type == models.ChatMemberTypeAdministrator ||
		member.Type == models.ChatMemberTypeOwner, nil
}
