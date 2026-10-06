package vmmemory

// Serving a migration's destination and handing a region off, over the
// zircon core, as serve.go does for the current core: the pages a region
// holds are its bindings' pages, resident or spilled, and those that are its
// own dirty state are what no checkpoint has.
