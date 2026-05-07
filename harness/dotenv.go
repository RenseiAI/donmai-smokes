package harness

import "github.com/joho/godotenv"

// LoadDotEnvLocal sources the named dotenv file (or `.env.local` by
// default) into the process environment so subsequent os.Getenv reads
// see the values.
//
// Existing process-env values win — godotenv.Load (not Overload) only
// sets keys that are not already present, so CI overrides remain
// authoritative. Absence of the file is silent (it's a dev convenience,
// not a requirement).
//
// Pass zero arguments to load `.env.local` from the current working
// directory; pass one or more paths to load specific files in order.
func LoadDotEnvLocal(paths ...string) error {
	if len(paths) == 0 {
		paths = []string{".env.local"}
	}
	return godotenv.Load(paths...)
}
