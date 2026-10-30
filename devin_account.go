package pacelever

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Devin's CLI stores the login separately from its selected organization. The
// organization is the non-secret identity; the API key only proves a login is
// present and is never hashed or returned. Both paths follow the CLI's XDG roots.
func devinAccountFP() string {
	var credentials struct {
		APIKey string `toml:"windsurf_api_key"`
	}
	body, err := os.ReadFile(filepath.Join(dataBase(), "devin", "credentials.toml"))
	if err != nil || toml.Unmarshal(body, &credentials) != nil || strings.TrimSpace(credentials.APIKey) == "" {
		return ""
	}
	config := readObject(filepath.Join(configBase(), "devin", "config.json"))
	org := strings.TrimSpace(str(object(config["devin"])["org_id"]))
	return accountFP("devin", org)
}
