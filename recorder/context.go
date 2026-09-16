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

// User is the person a piece of work is for.
//
// The identifier is what every record carries, and it is the whole of what a
// report can group by. The name and email are what a person reading that
// report needs to know who the identifier is, and they travel differently: not
// on any record, but once per person to a directory the report is joined to.
type User struct {
	// ID is the identifier the product's own sign-in issued, exactly as that
	// sign-in names it, without a `users/` prefix or any other path. It must
	// not contain a slash: it becomes the last segment of a resource name.
	ID string

	// Name is the person's name as the product shows it. Optional.
	Name string

	// Email is the person's email address. Optional.
	Email string
}

// named reports whether there is anything to say about the person beyond
// the identifier.
func (u User) named() bool {
	return u.Name != "" || u.Email != ""
}

// WithUser marks a context as belonging to one person.
//
// Set it where the sign-in has been checked, which is the one place a product
// has the identifier and the name in hand together. Only the identifier
// reaches a record; a name given here is sent once to the directory and never
// again until it changes.
func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, userContextKey{}, user)
}

// UserFrom returns the user a context carries, if any.
func UserFrom(ctx context.Context) User {
	user, _ := ctx.Value(userContextKey{}).(User)
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
