// Package testresource supplies roomy host resource allotments for tests whose
// subject is another invariant. Capacity tests supply their own small budget.
package testresource

import (
	"github.com/semistrict/sproutfs/resource"
)

func New() *resource.Budget {
	budget, err := resource.New(1 << 40)
	if err != nil {
		panic(err)
	}
	return budget
}
