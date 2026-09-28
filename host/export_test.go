package host

// SetGiveBackPages makes a give-back pass, and so a pass's worth of copies,
// pages long, and returns what puts the bound back.
func SetGiveBackPages(pages int) (restore func()) {
	was := giveBackPages
	giveBackPages = pages
	return func() { giveBackPages = was }
}
