package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
)

func main() {
	fmt.Println("deptrack")

	// TODO: accept cli argument
	rInput := reposForOrgInput{
		Name: "terraform-providers",
	}
	repos, err := reposForOrg(&rInput)
	if err != nil {
		fmt.Printf("Error: %s", err)
		os.Exit(1)
	}

	// repos = repos[:5]

	repoChan := make(chan *repoDepResult, len(repos))
	resultsChan := make(chan *repoDepResult, len(repos))

	concurrency := 3
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
				Org:  rInput.Name,
				Name: *r.Name,
			}
		}
		close(repoChan)
	}()

	fmt.Println("Running...")
	wg.Wait()
	p.Wait()
	close(resultsChan)
	var results []*repoDepResult
	for r := range resultsChan {
		results = append(results, r)
	}

	depMap := make(map[string][]string)
	for _, r := range results {
		if r.Deps != nil && len(r.Deps.Packages) > 0 {
			for _, d := range r.Deps.Packages {
				key := d.String()
				depMap[key] = append(depMap[key], r.FullName())
			}
		}
	}

	// save to file
	f, err := os.OpenFile("dep_result.csv", os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		fmt.Printf("Error saving file: %s", err)
		os.Exit(1)
	}
	defer f.Close()

	for k, repos := range depMap {
		// fmt.Printf("%s,%s\n", k, strings.Join(repos, ","))
		f.WriteString(fmt.Sprintf("%s,%s\n", k, strings.Join(repos, ",")))
	}
}

// repoDepResult stores information on a dependency
type repoDepResult struct {
	Org  string
	Name string
	Deps *vendorDeps
	err  error
}

func (r *repoDepResult) FullName() string {
	return fmt.Sprintf("%s/%s", r.Org, r.Name)
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
	key := fmt.Sprintf("%s_%s", d.Path, d.Revision)
	if d.Version != "" {
		key = key + "_" + d.Version
	}
	if d.VersionExact != "" {
		key = key + "_" + d.VersionExact
	}
	return key
}

func fetchVendor(wg *sync.WaitGroup, bar *mpb.Bar, repoChan <-chan *repoDepResult, resultsChan chan<- *repoDepResult) {
	// create client
	client := cleanhttp.DefaultClient()
	for r := range repoChan {
		// raw file url
		// https://raw.githubusercontent.com/terraform-providers/terraform-provider-aws/master/vendor/vendor.json
		url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/master/vendor/vendor.json", r.Org, r.Name)
		// log.Printf("\nurl: %s\n", url)

		// Submit the request
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("\nFailed to GET (%s): %s", url, err)
			r.err = fmt.Errorf("railed to GET (%s): %s", url, err)
			bar.Increment()
			resultsChan <- r
			continue
		}

		// Check the response
		if resp.StatusCode != http.StatusOK {
			r.err = fmt.Errorf("NotOK status for (%s/%s): %s", r.Org, r.Name, resp.Status)
			bar.Increment()
			resultsChan <- r
			continue
		}

		var deps vendorDeps
		if err := json.NewDecoder(resp.Body).Decode(&deps); err != nil {
			resp.Body.Close()
			log.Printf("\n\tfailed to parse response for (%s/%s): %s", r.Org, r.Name, err)
		}
		resp.Body.Close()
		r.Deps = &deps

		bar.Increment()
		resultsChan <- r
	}
	wg.Done()
}

// reposForOrgInput is used to provide options when searching repos for an org.
// Right now just supports name.
type reposForOrgInput struct {
	Name string
}

// reposForOrg returns all repos for an org. If input is nil, uses default org
// of Terraform Providers
func reposForOrg(input *reposForOrgInput) ([]*github.Repository, error) {
	key := os.Getenv("GITHUB_API_TOKEN")
	if key == "" {
		return nil, fmt.Errorf("missing API Token")
	}
	if input == nil || input.Name == "" {
		input = &reposForOrgInput{Name: "terraform-providers"}
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
	nopt := &github.RepositoryListByOrgOptions{}
	var repos []*github.Repository
	for {
		part, resp, err := client.Repositories.ListByOrg(ctx, input.Name, nopt)

		if err != nil {
			return nil, fmt.Errorf("error listing Repositories: %s", err)
		}
		repos = append(repos, part...)
		if resp.NextPage == 0 {
			break
		}
		nopt.Page = resp.NextPage
	}
	return repos, nil
}
