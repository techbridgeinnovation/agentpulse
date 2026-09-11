package recorder

import "context"

// Who a piece of work is for travels on the context rather than through every
// function between the handler and the model call.
//
// A service that threads these as parameters ends up passing three arguments
// through code that has no other reason to know about them, and the ones deepest
// down are the ones that get dropped. Set them once where a request starts.

type requestContextKey struct{}
type userContextKey struct{}
type sessionContextKey struct{}

// WithRequest marks a context as belonging to one end-user request.
//
// Everything recorded under this context, in this service or one it delegates
// to, groups together. That is what makes cost per unit of business work
// possible: a shortlist is produced by a request, not by a model call.
func WithRequest(ctx context.Context, request string) context.Context {
	return context.WithValue(ctx, requestContextKey{}, request)
}

// RequestFrom returns the request a context carries, if any.
func RequestFrom(ctx context.Context) string {
	request, _ := ctx.Value(requestContextKey{}).(string)
	return request
}

// WithUser marks a context as belonging to one person.
//
// An identifier, never an email and never a name. What reaches a record is what
// reaches a report, and a report is read by people who are not that person.
func WithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userContextKey{}, user)
}

// UserFrom returns the user a context carries, if any.
func UserFrom(ctx context.Context) string {
	user, _ := ctx.Value(userContextKey{}).(string)
	return user
}

// WithSession marks a context as belonging to one conversation.
func WithSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

// SessionFrom returns the session a context carries, if any.
func SessionFrom(ctx context.Context) string {
	session, _ := ctx.Value(sessionContextKey{}).(string)
	return session
}
