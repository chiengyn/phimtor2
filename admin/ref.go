package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ref.go parses the admin's input — either a bare TMDB id or a themoviedb.org
// link — into a (type, id) pair.
//
//   https://www.themoviedb.org/movie/27205-inception  -> ("movie", 27205)
//   https://www.themoviedb.org/tv/1399                 -> ("tv", 1399)
//   27205 (with explicit type from the UI dropdown)     -> (type, 27205)

var linkRe = regexp.MustCompile(`themoviedb\.org/(movie|tv)/(\d+)`)

// parseRef resolves the reference. typeHint is the UI's movie/tv selection and
// is only used when ref is a bare numeric id (links carry their own type).
func parseRef(ref, typeHint string) (mediaType string, id int, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, fmt.Errorf("empty reference")
	}

	if m := linkRe.FindStringSubmatch(ref); m != nil {
		id, _ := strconv.Atoi(m[2])
		return m[1], id, nil
	}

	if n, convErr := strconv.Atoi(ref); convErr == nil {
		if typeHint != "movie" && typeHint != "tv" {
			return "", 0, fmt.Errorf("a numeric id needs a type (movie or tv)")
		}
		return typeHint, n, nil
	}

	return "", 0, fmt.Errorf("not a TMDB id or themoviedb.org link: %q", ref)
}
