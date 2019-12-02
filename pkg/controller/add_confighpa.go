package controller

import (
	"github.com/iyacontrol/config-hpa-controller/pkg/controller/confighpa"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, confighpa.Add)
}
