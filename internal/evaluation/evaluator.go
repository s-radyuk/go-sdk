package evaluation

import (
	"fmt"
	"statsig/pkg/types"
)

type Evaluator struct {
	store      *Store
	ip3Country *interface{}
}

type gateEvalResult struct {
	value bool
}

type configEvalResult struct {
}

func New(secret string) *Evaluator {
	store := initStore(secret)

	// TODO: init ip3country
	fmt.Println("fuck")
	return &Evaluator{
		store:      store,
		ip3Country: nil,
	}
}

func (e Evaluator) CheckGate(user types.StatsigUser, gateName string) {
	// e.specStore.FeatureGates
	fmt.Println(gateName)
	fmt.Println(user)
	fmt.Println(e.store.FeatureGates[gateName])
}
