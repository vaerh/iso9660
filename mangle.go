package iso9660

import (
	"path"
	"strings"
)

func manglePath(input string) (string, string) {
	nonEmptySegments := splitPath(path.Clean(input))

	dirSegments := nonEmptySegments[:len(nonEmptySegments)-1]
	name := nonEmptySegments[len(nonEmptySegments)-1]

	for i := 0; i < len(dirSegments); i++ {
		dirSegments[i] = mangleDirectoryName(dirSegments[i])
	}
	name = mangleFileName(name)

	return path.Join(dirSegments...), name
}

func splitPath(input string) []string {
	rawSegments := strings.Split(input, "/")
	var nonEmptySegments []string
	for _, s := range rawSegments {
		if len(s) > 0 {
			nonEmptySegments = append(nonEmptySegments, s)
		}
	}
	return nonEmptySegments
}

// See ECMA-119 7.5
func mangleFileName(input string) string {
	input = strings.ToUpper(input)
	split := strings.Split(input, ".")

	version := "1"
	var filename, extension string
	if len(split) == 1 {
		filename = split[0]
	} else {
		filename = strings.Join(split[:len(split)-1], "_")
		extension = split[len(split)-1]
	}

	// enough characters for the `.ignition` extension
	extension = mangleDString(extension, 8)

	maxRemainingFilenameLength := primaryVolumeFileIdentifierMaxLength - (1 + len(version))
	if len(extension) > 0 {
		maxRemainingFilenameLength -= (1 + len(extension))
	}

	filename = mangleDString(filename, maxRemainingFilenameLength)

	if len(extension) > 0 {
		return filename + "." + extension + ";" + version
	}

	return filename + ";" + version
}

// See ECMA-119 7.6
func mangleDirectoryName(input string) string {
	return mangleDString(input, primaryVolumeDirectoryIdentifierMaxLength)
}

func mangleDString(input string, maxCharacters int) string {
	input = strings.ToUpper(input)

	var mangledString string
	for i := 0; i < len(input) && i < maxCharacters; i++ {
		r := rune(input[i])
		if strings.ContainsRune(dCharacters, r) {
			mangledString += string(r)
		} else {
			mangledString += "_"
		}
	}

	return mangledString
}
