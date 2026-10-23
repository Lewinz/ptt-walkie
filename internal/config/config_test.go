package config

import "testing"

func TestKodoRegion(t *testing.T) {
	cases := map[string]string{
		"https://s3.cn-south-1.qiniucs.com": "cn-south-1",
		"s3.ap-southeast-1.qiniucs.com":     "ap-southeast-1",
		"https://example.com":               "cn-east-1",
	}
	for ep, want := range cases {
		if got := (Config{KodoEndpoint: ep}).KodoRegionOrDefault(); got != want {
			t.Errorf("%s → %s, want %s", ep, got, want)
		}
	}
	if (Config{KodoEndpoint: "x", KodoRegion: "cn-north-1"}).KodoRegionOrDefault() != "cn-north-1" {
		t.Error("explicit region")
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList("事故,,封路,"); len(got) != 2 || got[1] != "封路" {
		t.Fatalf("%v", got)
	}
}
