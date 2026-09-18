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
type workspaceContextKey struct{}
type projectContextKey struct{}

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

// WithWorkspace marks a context as belonging to one tenant of the organisation.
//
// The value is the bare identifier the organisation knows that tenant by, `acme` and not `organisations/dealade/workspaces/acme`, for the same reason User.ID is bare: the organisation is already configured once where the records are sent, so the library holds both halves of the name and builds it, and there is one shape of it rather than one per call site.
//
// Set it where the product's sign-in is checked, beside the user, because that is the one place a product has the tenant and the person in hand at the same time. It is deliberately not an argument at a model call site: a value a call site can choose is a value that can attribute one tenant's spend to another, and a figure that is wrong that way looks exactly like a figure that is right.
//
// A context that carries no workspace is not a gap. A product with one tenant sets nothing, its records name no workspace, and they are filed under the organisation.
func WithWorkspace(ctx context.Context, workspace string) context.Context {
	return context.WithValue(ctx, workspaceContextKey{}, workspace)
}

// WorkspaceFrom returns the workspace a context carries, if any.
func WorkspaceFrom(ctx context.Context) string {
	workspace, _ := ctx.Value(workspaceContextKey{}).(string)
	return workspace
}

// WorkspaceName joins an organisation and a bare workspace identifier into the name the platform knows that workspace by.
//
// Here rather than at each of the places that needs one, so the name a record is filed under and the name a spend decision is asked about cannot be built two different ways.
//
// An empty workspace has no name, and the caller decides what that means: a batch with no workspace is filed under the organisation, and a decision with no workspace is asked about the organisation's default. Neither is this function inventing a name for something the caller did not name.
func WorkspaceName(organisation, workspace string) string {
	if workspace == "" {
		return ""
	}
	return organisation + "/workspaces/" + workspace
}

// WithProject marks a context as belonging to one unit of work inside the workspace, such as a refresh run or a matter.
//
// Set once beside the workspace and the user, and for the same reason. A product with no such concept sets nothing.
//
// A label and never a boundary: nothing is authorised against it, and everyone who can read the workspace can read every project in it. A project is not a way to keep one reader away from another's figures — that is what the workspace is.
func WithProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectContextKey{}, project)
}

// ProjectFrom returns the project a context carries, if any.
func ProjectFrom(ctx context.Context) string {
	project, _ := ctx.Value(projectContextKey{}).(string)
	return project
}
