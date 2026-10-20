# CloudMuX

CloudMuX is an in-process Multi-Cloud Library written entirely in Golang. It provides consistent interfaces to access OpenAPIs of cloud providers. Cloudpods CMP service is built upon CloudMux.

This fork is trimmed down to a single provider: **H3C CAS** (`pkg/multicloud/cas`). All other public cloud, private cloud and object storage providers have been removed.

## Command line tool -- cmx

cmx is the command line tool based on CloudMux. cmx is a compact utility to access H3C CAS.

### Complile

```ba### Example

`--cloud-env` is the CAS access URL; `--access-key` / `--secret` are the CAS username and password.

1. List regions

```bash
$ ./_output/bin/cmx --provider CAS --cloud-env http://$cas_host:8080 --access-key $username --secret $password region-list
```

2. List zones

```bash
$ ./_output/bin/cmx --provider CAS --cloud-env http://$cas_host:8080 --access-key $username --secret $password zone-list
```

3. List VM instances

```bash
$ ./_output/bin/cmx --provider CAS --cloud-env http://$cas_host:8080 --access-key $username --secret $password --region $region_id instance-list --zone $zone_id
```
