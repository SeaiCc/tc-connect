package core

import (
	"context"
)

// platform实现来接收一个terminal observation 消息, 当前只有
type ObserverTarget interface {
	SendObservation(ctx context.Context, channelID, text string) error
}
