package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/pkg/errors"
)

var PATTERN = regexp.MustCompile(`\b (\S+.+):(.+)@(sha256:[[:alnum:]]+)`) // https://regex101.com/r/hP8bK1/39

var constraintArgs rawConstraints

func main() {
	flag.Usage = func() {
		fmt.Printf(`update-docker-tags

Usage:
	update-docker-tags [options] < FILE | FOLDER >...

Options:
	--constraint  (repeatable) enforce a semver constraint for a given docker image
	--pattern     specify a custom regexp to match docker image tags

Examples:

	Update all image tags in a directory:

	$ update-docker-tags dir/

	Update all image tags in the given files and folders, enforcing constraints:

	$ update-docker-tags --constraint=ubuntu=<18.04 --constraint=alpine=<3.10 deployment.yaml dir/ 
`)
		os.Exit(2)
	}
	flag.Var(&constraintArgs, "constraint", "(repeatable) add a semver constraint for a given docker image")
	customPattern := flag.String("pattern", "", "specify a custom regexp to match docker images")
	flag.Parse()

	parsedConstraints, err := constraintArgs.parse()
	if err != nil {
		log.Fatalf("failed to parse raw constraints, err: %s", err)
	}

	paths := flag.Args()
	if len(paths) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var tagPattern *regexp.Regexp
	if *customPattern != "" {
		var err error
		tagPattern, err = regexp.Compile(*customPattern)
		if err != nil {
			log.Fatalf("failed to parse custom regex: %s", err)
		}
	} else {
		tagPattern = PATTERN
	}

	o := &options{
		constraints: parsedConstraints,
		filePaths:   paths,
		tagPattern:  tagPattern,
	}

	for _, root := range o.filePaths {
		if err := updateDockerTags(o, root); err != nil {
			log.Fatalf("failed to update docker tags for root %q, err: %s", root, err)
		}
	}

}

// UpdateDockerTags updates the Docker tags for the entire file tree rooted at
// "root" using the provided constraints.
func updateDockerTags(o *options, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}

		if strings.HasPrefix(path, ".git") {
			// Highly doubt anyone would ever want us to traverse git directories.
			return nil
		}

		data, err := ioutil.ReadFile(path)
		if err != nil {
			return errors.Wrap(err, "when reading file contents")
		}

		printedPath := false

		// replaceErr is a workaround for replaceAllSubmatchFunc not propagating errors
		var replaceErr error
		data = replaceAllSubmatchFunc(o.tagPattern, data, func(groups [][]byte) [][]byte {

			repositoryName := string(groups[0])
			repositoryName = isLibraryRepo(repositoryName)
			repository, err := newRepository(o, repositoryName)
			if err != nil {
				replaceErr = errors.Wrapf(err, "when initializing repository %q", repositoryName)
				return groups
			}

			repository.originalTag = string(groups[1])
			var newTag string

			if isNonSemverTag(repository.originalTag) {
				newTag = repository.originalTag
			} else {
				latest, err := repository.findLatestSemverTag()
				if err != nil {
					replaceErr = errors.Wrapf(err, "when finding the latest semver tag for '%s:%s'", repository.name, repository.originalTag)
					return groups
				}

				newTag = latest
			}

			newDigest, err := repository.fetchImageDigest(newTag)
			if err != nil {
				replaceErr = errors.Wrapf(err, "when fetching image digest for '%s:%s'", repository.name, newTag)
				return groups
			}

			if !printedPath {
				printedPath = true
				fmt.Println(path)
			}

			fmt.Println("\t", repository.name, "\t\t", newTag)
			groups[1] = []byte(newTag)
			groups[2] = []byte(newDigest)

			return groups
		}, -1)

		if replaceErr != nil {
			return errors.Wrapf(replaceErr, "when replacing image tags in %q", path)
		}

		err = ioutil.WriteFile(path, data, info.Mode())
		return errors.Wrapf(err, "when writing file contents of %q", path)
	})
}

type repository struct {
	name        string
	constraint  *semver.Constraints
	originalTag string

	authToken string
}

func isNonSemverTag(tag string) bool {
	_, err := semver.NewVersion(tag)

	// Assume that "tag" isn't a semver one (like "latest")
	// if we're unable to parse it
	return err != nil
}

func (r *repository) findLatestSemverTag() (string, error) {
	var versions []*semver.Version
	tags, err := r.fetchAllTags()
	if err != nil {
		return "", errors.Wrap(err, "when fetching all tags")
	}

	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil {
			continue // ignore non-semver tags
		}
		if v.Minor() == 0 { //might just want to exclude alpine all together
			continue //ignore values like 123124123.0.0
		}

		if r.constraint != nil {
			if r.constraint.Check(v) {
				versions = append(versions, v)
			}
		} else {
			versions = append(versions, v)
		}
	}

	if len(versions) == 0 {
		return "", fmt.Errorf("no semver tags found for %q", r.name)
	}

	versions = filterFlavor(r.originalTag, versions)

	sort.Sort(sort.Reverse(semver.Collection(versions)))
	latestTag := versions[0].Original()
	return latestTag, nil
}

func filterFlavor(tag string, ver semver.Collection) semver.Collection {
	s := strings.Split(tag, "-")
	if len(s) <= 1 { //no flavor, no-op
		return ver
	}
	if len(s) > 2 {
		log.Println("too many \"-\" to determine if this image has a flavor, check results")
		return ver
	}
	flavor := s[1]
	var vs semver.Collection
	for _, v := range ver {
		if strings.Contains(v.String(), flavor) {
			vs = append(vs, v)
		}
	}
	return vs
}

// Effectively the same as:
//
//  $ curl -s -D - -H "Authorization: Bearer $token" -H "Accept: application/vnd.docker.distribution.manifest.v2+json" https://index.docker.io/v2/sourcegraph/server/manifests/3.12.1 | grep Docker-Content-Digest
//
func (r *repository) fetchImageDigest(tag string) (string, error) {
	registry, repo, err := parseRegistry(r.name)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("GET", registry+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	if registry == "https://index.docker.io/v2/" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.authToken))
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return resp.Header.Get("Docker-Content-Digest"), nil
}
func parseRegistry(reg string) (registry, repo string, err error) {
	val := strings.Split(reg, "/")
	if len(val) < 3 {
		return "https://index.docker.io/v2/", reg, nil
	} else if len(val) > 3 {
		return "", "", fmt.Errorf("parsing error, expected \" %s \" to contain only 3 / ", reg)
	}
	return "https://" + val[0] + "/v2/", val[1] + "/" + val[2], nil
}

// Effectively the same as:
//
// 	$ export token=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:sourcegraph/server:pull" | jq -r .token)
//
func fetchAuthToken(repositoryName string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repositoryName))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	result := struct {
		Token string
	}{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", err
	}
	return result.Token, nil
}

// Effectively the same as:
//
// 	$ curl -H "Authorization: Bearer $token" https://index.docker.io/v2/sourcegraph/server/tags/list
//
func (r *repository) fetchAllTags() ([]string, error) {
	registry, repo, err := parseRegistry(r.name)
	req, err := http.NewRequest("GET", fmt.Sprintf("%s%s/tags/list", registry, repo), nil)
	if err != nil {
		return nil, err
	}
	if registry == "https://index.docker.io/v2/" {
		req.Header.Set("Authorization", "Bearer "+r.authToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result := struct {
		Tags []string
	}{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return nil, err
	}
	return result.Tags, nil
}

// replaceAllSubmatchFunc is the missing regexp.ReplaceAllSubmatchFunc; to use it:
//
// 	pattern := regexp.MustCompile(...)
// 	data = replaceAllSubmatchFunc(pattern, data, func(groups [][]byte) [][]byte {
// 		// mutate groups here
// 		return groups
// 	})
//
// This snippet is MIT licensed. Please cite by leaving this comment in place. Find
// the latest version at:
//
//  https://gist.github.com/slimsag/14c66b88633bd52b7fa710349e4c6749
//
func replaceAllSubmatchFunc(re *regexp.Regexp, src []byte, repl func([][]byte) [][]byte, n int) []byte {
	var (
		result  = make([]byte, 0, len(src))
		matches = re.FindAllSubmatchIndex(src, n)
		last    = 0
	)
	for _, match := range matches {
		// Append bytes between our last match and this one (i.e. non-matched bytes).
		matchStart := match[0]
		matchEnd := match[1]
		result = append(result, src[last:matchStart]...)
		last = matchEnd

		// Determine the groups / submatch bytes and indices.
		groups := [][]byte{}
		groupIndices := [][2]int{}
		for i := 2; i < len(match); i += 2 {
			start := match[i]
			end := match[i+1]
			groups = append(groups, src[start:end])
			groupIndices = append(groupIndices, [2]int{start, end})
		}

		// Replace the groups as desired.
		groups = repl(groups)

		// Append match data.
		lastGroup := matchStart
		for i, newValue := range groups {
			// Append bytes between our last group match and this one (i.e. non-group-matched bytes)
			groupStart := groupIndices[i][0]
			groupEnd := groupIndices[i][1]
			result = append(result, src[lastGroup:groupStart]...)
			lastGroup = groupEnd

			// Append the new group value.
			result = append(result, newValue...)
		}
		result = append(result, src[lastGroup:matchEnd]...) // remaining
	}
	result = append(result, src[last:]...) // remaining
	return result
}

type rawConstraint struct {
	image      string
	constraint string
}

func (rc *rawConstraint) String() string {
	return fmt.Sprintf("%s=%s", rc.image, rc.constraint)
}

type rawConstraints []*rawConstraint

func (rc *rawConstraints) String() string {
	var elems []string
	for _, raw := range *rc {
		elems = append(elems, raw.String())
	}
	return strings.Join(elems, ", ")
}

func (rc *rawConstraints) Set(value string) error {
	splits := strings.Split(value, "=")
	if len(splits) != 2 {
		return fmt.Errorf("unable to split constraint %q", value)
	}

	image, constraint := splits[0], splits[1]
	*rc = append(*rc, &rawConstraint{
		image:      image,
		constraint: constraint,
	})
	return nil
}

func (rc *rawConstraints) parse() (map[string]*semver.Constraints, error) {
	out := map[string]*semver.Constraints{}
	for _, raw := range *rc {
		parsed, err := semver.NewConstraint(raw.constraint)
		if err != nil {
			return nil, fmt.Errorf("cannot parse constraint %q, err: %w", raw.constraint, err)
		}

		out[raw.image] = parsed
	}
	return out, nil
}

type options struct {
	constraints map[string]*semver.Constraints
	filePaths   []string
	tagPattern  *regexp.Regexp
}

func newRepository(o *options, repositoryName string) (*repository, error) {
	token, err := fetchAuthToken(repositoryName)
	if err != nil {
		return nil, errors.Wrap(err, "when fetching auth token")
	}
	return &repository{
		name:       repositoryName,
		constraint: o.constraints[repositoryName],

		authToken: token,
	}, nil
}

// isLibraryRepo checks if the repository is a "library" style repo; doesn't have a "/"
func isLibraryRepo(repositoryName string) string {
	if strings.Contains(repositoryName, "/") {
		return repositoryName
	}
	return "library/" + repositoryName
}
