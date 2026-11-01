package main

// Tokyo Night colors, from folke/tokyonight.nvim (Apache-2.0):
//   night: lua/tokyonight/colors/storm.lua, overridden by colors/night.lua
//   day:   colors/day.lua = night inverted in HSLuv (L' = 100-L, then
//          L' += (100-L')*0.3 when L' < 40; util.invert, day_brightness 0.3),
//          bg_dark = blend(bg, 0.9, fg). Values below were computed with that
//          algorithm and match the published Tokyo Night Day colors.

var night = map[string]string{
	"bg": "#1a1b26", "bg_dark": "#16161e", "bg_dark1": "#0c0e14", "bg_highlight": "#292e42",
	"blue": "#7aa2f7", "blue0": "#3d59a1", "blue1": "#2ac3de", "blue2": "#0db9d7",
	"blue5": "#89ddff", "blue6": "#b4f9f8", "blue7": "#394b70", "comment": "#565f89",
	"cyan": "#7dcfff", "dark3": "#545c7e", "dark5": "#737aa2", "fg": "#c0caf5",
	"fg_dark": "#a9b1d6", "fg_gutter": "#3b4261", "green": "#9ece6a", "green1": "#73daca",
	"green2": "#41a6b5", "magenta": "#bb9af7", "magenta2": "#ff007c", "orange": "#ff9e64",
	"purple": "#9d7cd8", "red": "#f7768e", "red1": "#db4b4b", "teal": "#1abc9c",
	"terminal_black": "#414868", "yellow": "#e0af68",
}

var day = map[string]string{
	"bg": "#e1e2e7", "bg_dark": "#d0d5e3", "bg_dark1": "#c1c9df", "bg_highlight": "#c4c8da",
	"blue": "#2e7de9", "blue0": "#7890dd", "blue1": "#188092", "blue2": "#07879d",
	"blue5": "#006a83", "blue6": "#2e5857", "blue7": "#92a6d5", "comment": "#848cb5",
	"cyan": "#007197", "dark3": "#8990b3", "dark5": "#68709a", "fg": "#3760bf",
	"fg_dark": "#6172b0", "fg_gutter": "#a8aecb", "green": "#587539", "green1": "#387068",
	"green2": "#38919f", "magenta": "#9854f1", "magenta2": "#d20065", "orange": "#b15c00",
	"purple": "#7847bd", "red": "#f52a65", "red1": "#c64343", "teal": "#118c74",
	"terminal_black": "#a1a6c5", "yellow": "#8c6c3e",
}

// hueMap: Web Awesome hue -> Tokyo Night color name (same name in night and day).
// Chosen by OKLCH hue angle (the generator prints them). Tokyo Night has no
// real indigo: its two violets (purple, magenta) share one hue, so indigo
// takes blue0, the deep blue Tokyo Night uses for selection and search.
// extra adds more exact anchors to a hue ("day.fg" = Tokyo Night Day's text color).
var hueMap = []struct {
	wa, tn string
	extra  []string
}{
	{"red", "red", nil},                  // H 10
	{"orange", "orange", nil},            // H 51
	{"yellow", "yellow", nil},            // H 75
	{"green", "green", nil},              // H 130
	{"cyan", "cyan", nil},                // H 236
	{"blue", "blue", []string{"day.fg"}}, // H 264; day.fg #3760bf is a blue too
	{"indigo", "blue0", nil},             // H 265, darker
	{"purple", "magenta", nil},           // H 300
	{"pink", "magenta2", nil},            // H 4
}

// grayAnchors: Tokyo Night's UI neutrals that become exact gray tints,
// dark ones from night, light ones from day.
var grayAnchors = []struct{ set, name string }{
	{"night", "bg_dark"},
	{"night", "bg"},
	{"night", "bg_highlight"},
	{"night", "fg_gutter"},
	{"night", "dark3"},
	{"night", "dark5"},
	{"night", "fg_dark"},
	{"night", "fg"},
	{"day", "bg"},
}
