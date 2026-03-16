package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/theme"
)

func main() {
	a := app.NewWithID("com.musicsync.app")
	a.Settings().SetTheme(theme.DarkTheme())

	w := a.NewWindow("MusicSync")
	w.Resize(fyne.NewSize(1100, 700))
	w.SetContent(buildUI(a, w))
	w.ShowAndRun()
}
