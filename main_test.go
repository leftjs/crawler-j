package main

import (
	"strings"
	"testing"
)

func Test_qbLogin(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		// TODO: Add test cases.
		{
			name:    "qbLogin",
			want:    "SID",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := qbLogin()
			if (err != nil) != tt.wantErr {
				t.Errorf("qbLogin() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !strings.Contains(got.String(), tt.want) {
				t.Errorf("qbLogin() got = %v, want %v", got, tt.want)
			}
		})
	}
}
