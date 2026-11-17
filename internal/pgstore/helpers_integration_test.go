//go:build integration

package pgstore_test

import "github.com/ricartefelipe/recorvia/internal/customers"

func customersFilter() customers.ListFilter { return customers.ListFilter{Limit: 100} }
