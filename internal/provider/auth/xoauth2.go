package auth

import (
	"errors"
	"fmt"
)

type XOAUTH2 struct {
	Username    string
	AccessToken string
	challenged  bool
}

func (c *XOAUTH2) Start() (string, []byte, error) {
	if c.Username == "" || c.AccessToken == "" {
		return "", nil, errors.New("XOAUTH2 credentials are empty")
	}
	response := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.Username, c.AccessToken)
	return "XOAUTH2", []byte(response), nil
}

func (c *XOAUTH2) Next(_ []byte) ([]byte, error) {
	if c.challenged {
		return nil, errors.New("unexpected repeated XOAUTH2 challenge")
	}
	c.challenged = true
	return []byte{}, nil
}
