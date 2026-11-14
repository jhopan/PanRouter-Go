package clientconfig

// Devin CLI owns no configuration file here.
//
// src/app/api/cli-tools/devin-settings/route.js only detects the binary (PATH,
// then the installer's locations) and reports its version: "the binary handles
// its own auth". There is no file to write, so Apply and Reset are the empty
// plan and succeed without touching the disk — the adapter exists so the
// dashboard route resolves and so `connect --tools` can name the client without
// inventing a file it must not touch.

type devinAdapter struct{ baseAdapter }

func newDevinAdapter() Adapter {
	return devinAdapter{baseAdapter{id: "devin", paths: devinPaths}}
}

func (a devinAdapter) Plan(Options) ([]FilePlan, error) { return nil, nil }

func (a devinAdapter) Apply(Options) (Result, error) { return Result{}, nil }

func (a devinAdapter) Reset(Options) (Result, error) { return Result{}, nil }
