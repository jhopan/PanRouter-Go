package clientconfig

// jcode: ~/.jcode/config.toml plus $XDG_CONFIG_HOME/jcode/provider-9router.env.
//
// src/app/api/cli-tools/jcode-settings/route.js writes the provider profile into
// the TOML file and the credential into a dotenv file the profile names through
// api_key_env. Two files, because jcode reads the key from the environment it
// loads from env_file, not from the profile.
//
// The env file uses the TOML writer: dotenv's KEY="value" lines are TOML root
// keys, so the same parser, the same verify pass and the same backup path apply.

// jcodeProfileKey is the provider table name (route.js: providers["9router"]).
const jcodeProfileKey = "9router"

// jcodeKeyEnv is the variable name the profile references and the env file sets.
const jcodeKeyEnv = "JCODE_9ROUTER_API_KEY"

// jcodeEnvFile is the file name the profile points at, relative to jcode's
// config directory.
const jcodeEnvFile = "provider-9router.env"

// jcodeDefaultModel is the route's fallback when the caller sends no models.
const jcodeDefaultModel = "cc/claude-opus-4-7"

type jcodeAdapter struct{ baseAdapter }

func newJCodeAdapter() Adapter {
	return jcodeAdapter{baseAdapter{id: "jcode", paths: jcodePaths}}
}

func (a jcodeAdapter) Plan(opts Options) ([]FilePlan, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return nil, err
	}
	defaultModel := jcodeDefaultModel
	if models := cliModels(opts); len(models) > 0 {
		defaultModel = models[0]
	}
	profile := []string{"providers", jcodeProfileKey}
	configEdits := []Edit{
		{KeyPath: appendPath(profile, "type"), Value: "openai-compatible", CreateIfMissing: true},
		{KeyPath: appendPath(profile, "base_url"), Value: withV1(opts.BaseURL), CreateIfMissing: true},
		{KeyPath: appendPath(profile, "auth"), Value: "bearer", CreateIfMissing: true},
		{KeyPath: appendPath(profile, "api_key_env"), Value: jcodeKeyEnv, CreateIfMissing: true},
		{KeyPath: appendPath(profile, "env_file"), Value: jcodeEnvFile, CreateIfMissing: true},
		{KeyPath: appendPath(profile, "default_model"), Value: defaultModel, CreateIfMissing: true},
		// A TOML boolean is a raw scalar; the quoted form would make the field a
		// string and jcode would read it as truthy either way, but the file
		// would no longer mean what the schema says.
		{KeyPath: appendPath(profile, "requires_api_key"), ValueJSON: "true", CreateIfMissing: true},
	}
	envEdits := []Edit{{KeyPath: []string{jcodeKeyEnv}, Value: cliAPIKey(opts.APIKey), CreateIfMissing: true}}
	return []FilePlan{
		{File: paths.Files[0], Edits: configEdits},
		{File: paths.Files[1], Edits: envEdits},
	}, nil
}

func (a jcodeAdapter) Apply(opts Options) (Result, error) {
	plans, err := a.Plan(opts)
	if err != nil {
		return Result{}, err
	}
	return applyPlans(plans, nil)
}

func (a jcodeAdapter) Reset(opts Options) (Result, error) {
	paths, err := a.Paths(opts)
	if err != nil {
		return Result{}, err
	}
	profile := []string{"providers", jcodeProfileKey}
	configEdits := deleteEdits(
		appendPath(profile, "type"),
		appendPath(profile, "base_url"),
		appendPath(profile, "auth"),
		appendPath(profile, "api_key_env"),
		appendPath(profile, "env_file"),
		appendPath(profile, "default_model"),
		appendPath(profile, "requires_api_key"),
	)
	// The credential goes in the same pass as the profile that references it:
	// leaving the key behind would keep a working credential in a file nothing
	// points at any more.
	envEdits := deleteEdits([]string{jcodeKeyEnv})
	return applyPlans([]FilePlan{
		{File: paths.Files[0], Edits: configEdits},
		{File: paths.Files[1], Edits: envEdits},
	}, nil)
}
