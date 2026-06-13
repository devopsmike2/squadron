// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"reflect"
	"testing"
)

func TestParseInventoryHosts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "user provided shape",
			in:   "[windows]\n#10.10.40.7\nGAXGPAP158UA\n",
			want: []string{"GAXGPAP158UA"},
		},
		{
			name: "multiple groups, vars section ignored",
			in: `[windows]
host01
host02.example.com

[windows:vars]
ansible_user=svc-deploy
ansible_password=hunter2

[linux]
linux01
linux02
`,
			want: []string{"host01", "host02.example.com", "linux01", "linux02"},
		},
		{
			name: "inline host vars: keep host, drop vars",
			in: `[windows]
host01 ansible_user=svc-deploy ansible_password=hunter2
host02
`,
			want: []string{"host01", "host02"},
		},
		{
			name: "comment forms (# and ;) and trailing comment",
			in: `; a leading comment
[windows]
# host00.example.com
host01   ; trailing comment
host02
`,
			want: []string{"host01", "host02"},
		},
		{
			name: "blank lines and dedupe (case-insensitive)",
			in: `[windows]

host01

HOST01

host02

`,
			want: []string{"host01", "host02"},
		},
		{
			name: "children section ignored",
			in: `[all:children]
windows
linux

[windows]
host01
`,
			want: []string{"host01"},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "only comments + sections",
			in: `# nothing here
[windows]
; commented away
[windows:vars]
ansible_user=x
`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseInventoryHosts([]byte(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseInventoryHosts mismatch\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}
