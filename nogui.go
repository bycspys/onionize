// nogui.go - empty GUI wrapper.
//
// To the extent possible under law, Ivan Markin waived all copyright
// and related or neighboring rights to this module of onionize, using the creative
// commons "cc0" public domain dedication. See LICENSE or
// <http://creativecommons.org/publicdomain/zero/1.0/> for full details.
// +build !gui

package main

import "log"

func guiMain() {
	log.Fatal("This version of onionize is built without GUI support")
}