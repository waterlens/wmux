package sshconfig

// newWithHome lets tests model the account home independently from the process
// environment without changing global state.
func newWithHome(path, home string) *Discoverer {
	return &Discoverer{path: path, homeOverride: home}
}

// processUsername is the default Username a Candidate inherits when the config
// declares no User.
func processUsername() (string, error) {
	account, err := runningAccount("")
	if err != nil {
		return "", err
	}
	return account.username, nil
}
