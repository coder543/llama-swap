package swaputil

import "context"

// RequestTurn coordinates admission of a request with earlier arrivals.
type RequestTurn interface {
	Wait(context.Context) bool
	Release()
}

type requestTurnKey struct{}

func WithRequestTurn(ctx context.Context, turn RequestTurn) context.Context {
	return context.WithValue(ctx, requestTurnKey{}, turn)
}

func HasRequestTurn(ctx context.Context) bool {
	_, ok := ctx.Value(requestTurnKey{}).(RequestTurn)
	return ok
}

func WaitRequestTurn(ctx context.Context) bool {
	if turn, ok := ctx.Value(requestTurnKey{}).(RequestTurn); ok {
		return turn.Wait(ctx)
	}
	return ctx.Err() == nil
}

func ReleaseRequestTurn(ctx context.Context) {
	if turn, ok := ctx.Value(requestTurnKey{}).(RequestTurn); ok {
		turn.Release()
	}
}
