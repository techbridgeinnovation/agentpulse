package recorder

import (
	"runtime/debug"
	"sync"
)

var modules = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Replace != nil {
				dep = dep.Replace
			}
			out[dep.Path] = dep.Version
		}
	}
	return out
})

// ModuleVersion is the version of a module built into this binary, read once, or empty where the build does not say.
func ModuleVersion(path string) string {
	return modules()[path]
}
