package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
)

// searchFilter is used to provide options when searching repos for an org.
type searchFilter struct {
	Organizations []string
	Repositories  []string
	Packages      []string
	Limit         int
	Concurrency   int
	ShowErrs      bool
}

func (s searchFilter) String() string {
	var orgs, repos string
	if len(s.Organizations) > 0 {
		if len(s.Organizations) > 3 {
			orgs = "many organizations"
		} else {
			orgs = fmt.Sprintf("(%s)", strings.Join(s.Organizations, ", "))
		}
	}
	if len(s.Repositories) > 0 {
		if len(s.Repositories) > 3 {
			orgs = "many repositories"
		} else {
			orgs = fmt.Sprintf("(%s)", strings.Join(s.Repositories, ", "))
		}
		return orgs + " and " + repos
	}
	return orgs
}

func main() {
	helpText := `
Usage: deptrack [options] 

  Gather dependencies from Go projects across your projects on GitHub.

Options:
  -o, --organizations 	Crawl all repositories an organization. Comma seperated list. Default "terraform-providers"
  -r, --repositories 	Crawl only these repositories (comman seperated list). Must be full name "org/repo"
  -l, --limit 		Limit number or repos to crawl. Default is to report on all repositories listed, or all found in the given organization
  -c, --concurrency 	Concurrency devel; Default 20
	-p, --packages 		Comma seperated list of packages to search for
  -e, --errors 		Flag to list repos that returned an error. Default false
  -h, --help 		Print this message. But you knew this by now
`

	// TODO: accept cli argument
	filter := searchFilter{
		Organizations: []string{"terraform-providers"},
		Concurrency:   20,
	}

	args := os.Args[1:]
	if len(args) > 0 {
		for i, a := range args {
			if a == "-h" || a == "-help" {
				fmt.Println(helpText)
				os.Exit(0)
			}
			if a == "-o" || a == "-organizations" {
				filter.Organizations = strings.Split(args[i+1], ",")
			}
			if a == "-r" || a == "-repositories" {
				filter.Repositories = strings.Split(args[i+1], ",")
			}
			if a == "-p" || a == "-packages" {
				filter.Packages = strings.Split(args[i+1], ",")
			}
			if a == "-l" || a == "-limit" {
				i, err := strconv.Atoi(args[i+1])
				if err != nil {
					fmt.Printf("[WARN] Error parsing limit value: %s", err)
					continue
				}
				filter.Limit = i
			}
			if a == "-c" || a == "-concurrency" {
				i, err := strconv.Atoi(args[i+1])
				if err != nil {
					fmt.Printf("[WARN] Error parsing concurrency value: %s", err)
					continue
				}
				filter.Concurrency = i
			}

			if a == "-e" || a == "-errors" {
				filter.ShowErrs = true
			}
		}
	}

	if len(filter.Organizations) == 0 && len(filter.Repositories) == 0 {
		fmt.Println("No organizations or repositories provided")
		os.Exit(1)
	}

	// fmt.Printf("\ndebug:\n%#v\n", filter)

	repos, err := reposForOrg(&filter)
	if err != nil {
		fmt.Printf("Error: %s", err)
		os.Exit(1)
	}

	if len(repos) == 0 {
		fmt.Println("No repositories found")
		os.Exit(0)
	}

	if filter.Limit > 0 {
		fmt.Println("Limiting to (%d) repositories", filter.Limit)
		repos = repos[:filter.Limit]
	}

	if len(repos) < filter.Concurrency {
		filter.Concurrency = len(repos)
	}

	repoChan := make(chan *repoDepResult, len(repos))
	resultsChan := make(chan *repoDepResult, len(repos))

	concurrency := filter.Concurrency
	var wg sync.WaitGroup
	wg.Add(concurrency)

	p := mpb.New(
		// override default "[=>-]" format
		mpb.WithFormat("╢▌▌░╟"),
		// override default 120ms refresh rate
		mpb.WithRefreshRate(80*time.Millisecond),
	)

	bar := p.AddBar(int64(len(repos)),
		mpb.PrependDecorators(
			decor.CountersNoUnit("Repos: %d / %d", 12, 0),
		),
		mpb.AppendDecorators(
			decor.Percentage(5, 0),
		),
	)

	for i := 0; i < concurrency; i++ {
		go fetchVendor(&wg, bar, repoChan, resultsChan)
	}

	go func() {
		for _, r := range repos {
			repoChan <- &repoDepResult{
				FullName: *r.FullName,
				Name:     *r.Name,
				Packages: filter.Packages,
			}
		}
		close(repoChan)
	}()

	fmt.Printf("Gathering dependencies for %s...\n", filter)
	wg.Wait()
	p.Wait()
	close(resultsChan)
	var results []*repoDepResult
	var errd []*repoDepResult
	for r := range resultsChan {
		if r.err != nil {
			errd = append(errd, r)
			continue
		}
		results = append(results, r)
	}
	bar.Complete()

	depMap := make(map[string][]string)
	for _, r := range results {
		if r.Deps != nil && len(r.Deps.Packages) > 0 {
			for _, d := range r.Deps.Packages {
				key := d.String()
				depMap[key] = append(depMap[key], r.RepoName())
			}
		}
	}

	if len(results) > 0 {
		// save to file
		fmt.Println("Saving to 'dependencies.csv'...")
		f, err := os.OpenFile("dependencies.csv", os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0644)
		if err != nil {
			fmt.Printf("Error saving file: %s", err)
			os.Exit(1)
		}
		defer f.Close()
		f.WriteString("Package,Revision,Version,Exact Version,Providers\n")
		var keys []string
		for k := range depMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			repos := depMap[k]
			// fmt.Printf("%s,%s\n", k, strings.Join(repos, ","))
			parts := make([]string, 4, 4)
			d := strings.Split(k, "::")
			// key format:
			// path::revision::version::version_exact
			// where version and version exact may not exist
			for i, s := range d {
				parts[i] = s
			}
			sort.Strings(repos)
			f.WriteString(fmt.Sprintf("%s,%s\n", strings.Join(parts, ","), strings.Join(repos, ",")))
		}
	}

	// show a summary
	fmt.Printf("\n\nResults found: %d\nError count: %d\n", len(results), len(errd))
	if len(errd) > 0 && filter.ShowErrs {
		fmt.Println("Repos with error status:")
		for _, r := range errd {
			fmt.Printf("- %s err: %s\n", r.FullName, r.err)
		}
	}

	fmt.Println("Done!")
}

// repoDepResult stores information on a dependency
type repoDepResult struct {
	FullName string
	Name     string
	Deps     *vendorDeps
	Packages []string
	err      error
}

func (r *repoDepResult) RepoName() string {
	return fmt.Sprintf("%s", r.Name)
}

type vendorDeps struct {
	Comment  string
	Ignore   string
	Packages []depPackage `json:"package"`
}

type depPackage struct {
	ChecksumSHA1 string
	Path         string
	Revision     string
	RevisionTime string
	Version      string
	VersionExact string
}

func (d *depPackage) String() string {
	key := fmt.Sprintf("%s::%s", d.Path, d.Revision)
	if d.Version != "" {
		key = key + "::" + d.Version
	}
	if d.VersionExact != "" {
		key = key + "::" + d.VersionExact
	}
	return key
}

func fetchVendor(wg *sync.WaitGroup, bar *mpb.Bar, repoChan <-chan *repoDepResult, resultsChan chan<- *repoDepResult) {
	// create client
	client := cleanhttp.DefaultClient()
	for r := range repoChan {
		// raw file url
		// https://raw.githubusercontent.com/terraform-providers/terraform-provider-aws/master/vendor/vendor.json
		url := fmt.Sprintf("https://raw.githubusercontent.com/%s/master/vendor/vendor.json", r.FullName)

		// Submit the request
		resp, err := client.Get(url)
		if err != nil {
			r.err = fmt.Errorf("railed to GET (%s): %s", url, err)
			bar.Increment()
			resultsChan <- r
			continue
		}

		// Check the response
		if resp.StatusCode != http.StatusOK {
			r.err = fmt.Errorf("%s", resp.Status)
			bar.Increment()
			resultsChan <- r
			continue
		}

		var deps vendorDeps
		if err := json.NewDecoder(resp.Body).Decode(&deps); err != nil {
			resp.Body.Close()
			r.err = fmt.Errorf("\n\tfailed to parse response for (%s): %s", r.FullName, err)
		}
		resp.Body.Close()

		// check if we're filtering packages
		if len(r.Packages) > 0 {
			var filteredDeps []depPackage
			for _, p := range r.Packages {
				for _, vp := range deps.Packages {
					if strings.Contains(vp.Path, p) {
						filteredDeps = append(filteredDeps, vp)
					}
				}
			}
			deps.Packages = filteredDeps
		}
		r.Deps = &deps

		bar.Increment()
		resultsChan <- r
	}
	wg.Done()
}

// reposForOrg returns all repos for an org. If filter is nil, uses default org
// of Terraform Providers
func reposForOrg(filter *searchFilter) ([]*github.Repository, error) {
	key := os.Getenv("GITHUB_API_TOKEN")
	if key == "" {
		return nil, fmt.Errorf("missing API Token")
	}

	// refactor, this is boilerplate
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: key},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	// get list of repositories across terraform-repositories, and add in
	// hashicorp/terraform
	var repos []*github.Repository
	for _, org := range filter.Organizations {
		nopt := &github.RepositoryListByOrgOptions{}
		for {
			part, resp, err := client.Repositories.ListByOrg(ctx, org, nopt)

			if err != nil {
				return nil, fmt.Errorf("error listing Repositories: %s", err)
			}
			repos = append(repos, part...)
			if resp.NextPage == 0 {
				break
			}
			nopt.Page = resp.NextPage
		}
	}
	return repos, nil
}
