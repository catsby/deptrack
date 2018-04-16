# deptrack
## STATUS: experimental hack-job

A fun hack project to record the golang deps from an Organizations repositories
based on `vendor/vendor.json`

## Usage

- requires `GITHUB_API_TOKEN`
- `$ git checkout https://github.com/catsby/deptrack`
- `$ go install`
- `$ deptrack`

![usage](https://dl.dropboxusercontent.com/s/cjx40kvpfezmknh/deptrack.gif)

## Output

Saves results in a comma seperated list to `dep_result.csv`. Format:

golang.org/x/net/context,6078986fec03a1dcc236c34816c71b0e05018fda,,,terraform-provider-cloudflare::

```
Package, Revision, Version, Exact version, Provider, Provider, Provider...
```

All results should have `Package` and `Revision`, but may not have `Version` or
`Exact Version`. Each result will have 1 or more `Provider` name listing the
providers that use this package


```
github.com/aws/aws-sdk-go/aws/request,b79a722cb7aba0edd9bd2256361ae2e15e98f8ad,v1.10.36,v1.10.36,terraform-provider-nsxt
golang.org/x/net/context,6078986fec03a1dcc236c34816c71b0e05018fda,,,terraform-provider-cloudflare
[...]
```
