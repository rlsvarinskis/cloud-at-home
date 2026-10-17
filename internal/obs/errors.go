package obs

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"example.com/homecloud/internal/auth"
	"example.com/homecloud/internal/errs"
)

// ErrorInterceptor converts any error a handler returns into a Connect error (see errs.ToConnect) and
// logs the ones that become Internal, with the caller and procedure, because the client only sees
// "internal error". Install it between the metrics interceptor and the handler.
func ErrorInterceptor(log *slog.Logger) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			res, err := next(ctx, req)
			if err == nil {
				return res, nil
			}
			ce := errs.ToConnect(err)
			var c *connect.Error
			if errors.As(ce, &c) && c.Code() == connect.CodeInternal && log != nil {
				who := "anonymous"
				if p, ok := auth.FromContext(ctx); ok {
					who = p.Subject
				}
				log.Error("internal error", "procedure", req.Spec().Procedure, "caller", who, "err", err)
			}
			return res, ce
		}
	})
}
