package recorder

import (
	"os"
	"strconv"
)

// DefaultRegion is where a framework's model calls are processed when the adopter does not say: the Vertex location the Google client is configured with, and nothing for other providers.
func DefaultRegion(billedBy string) string {
	if billedBy != ProviderVertexAI {
		return ""
	}
	if vertex, _ := strconv.ParseBool(os.Getenv("GOOGLE_GENAI_USE_VERTEXAI")); !vertex {
		return ""
	}
	return os.Getenv("GOOGLE_CLOUD_LOCATION")
}
