package main

import (
	"context"
	"log/slog"

	"github.com/akomyagin/aiTelegaBot/internal/config"
	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
)

// runLogin executes the one-shot interactive MTProto login (`bot login`).
// It reuses config.Load() for MTProto credentials and session settings.
func runLogin(ctx context.Context, cfg *config.Config) error {
	slog.Info("starting MTProto interactive login")
	return mtproto.Login(ctx, mtproto.MTProtoConfig{
		AppID:       cfg.MTProtoAppID,
		AppHash:     cfg.MTProtoAppHash,
		SessionPath: cfg.MTProtoSession,
		SessionKey:  cfg.MTProtoKey,
	})
}
