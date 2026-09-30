package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRepoFullName(t *testing.T) {
	r := Repo{Owner: "redscaresu", Name: "trailboss"}
	assert.Equal(t, "redscaresu/trailboss", r.FullName())
}
