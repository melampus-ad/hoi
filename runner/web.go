// Copyright 2014 Jim Studt. All rights reserved.
// Copyright 2015 tgic. All rights reserved.
// Copyright 2016 Atelier Disko. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package runner

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"math/rand"

	"github.com/atelierdisko/hoi/builder"
	pConfig "github.com/atelierdisko/hoi/config/project"
	sConfig "github.com/atelierdisko/hoi/config/server"
	"github.com/atelierdisko/hoi/system"
)

func NewWebRunner(s sConfig.Config, p pConfig.Config) *WebRunner {
	return &WebRunner{
		s:     s,
		p:     p,
		build: builder.NewScopedBuilder(builder.KIND_WEB, "servers/*.conf", p, s),
		sys:   system.NewNGINX(p, s),
	}
}

type WebRunner struct {
	s     sConfig.Config
	p     pConfig.Config
	sys   *system.NGINX
	build *builder.Builder
}

func (r WebRunner) Disable() error {
	servers, err := r.sys.ListInstalled()
	if err != nil {
		return err
	}
	for _, s := range servers {
		if err := r.sys.Uninstall(s); err != nil {
			return err
		}
	}
	if len(servers) == 0 {
		return nil // nothing to disable
	}
	return r.sys.Reload()
}

func (r WebRunner) Enable() error {
	if len(r.p.Cron) == 0 {
		return nil // nothing to do
	}
	files, err := r.build.ListAvailable()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := r.sys.Install(f); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return nil // nothing to enable
	}
	return r.sys.Reload()
}

func (r WebRunner) Clean() error {
	return r.build.Clean()
}

func (r WebRunner) Generate() error {
	if len(r.p.Domain) == 0 {
		return nil // nothing to do
	}

	creds, err := r.p.GetCreds()
	if err != nil {
		return err
	}
	if len(creds) != 0 {
		var tmp []byte
		buf := bytes.NewBuffer(tmp)

		// APR1-MD5 is the strongest hash nginx supports for basic auth
		salt := generateAPR1Salt()

		for user, password := range creds {
			buf.WriteString(fmt.Sprintf("%s:%s\n", user, computeAPR1(password, salt)))
		}
		if err := r.build.WriteSensitiveFile("password", buf); err != nil {
			return err
		}
	}
	for k, v := range r.p.Domain {
		if !v.SSL.IsEnabled() {
			continue
		}
		e := r.p.Domain[k]

		path, err := e.SSL.GetCertificate()
		if err != nil {
			return err
		}
		e.SSL.Certificate = path

		path, err = e.SSL.GetCertificateKey()
		if err != nil {
			return err
		}
		e.SSL.CertificateKey = path

		r.p.Domain[k] = e
	}

	tmplData := struct {
		P             pConfig.Config
		S             sConfig.Config
		WebConfigPath string
	}{
		P: r.p,
		S: r.s,
		// even though we symlink parts of the build path, config files
		// should not rely on symlinking but reference the original
		// created files
		WebConfigPath: r.build.Path(),
	}
	return r.build.LoadWriteTemplates(tmplData)
}

const APR1abc string = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// 8 byte long salt from APR1 alphabet
func generateAPR1Salt() string {
	b := make([]byte, 8)

	for i := range b {
		b[i] = APR1abc[rand.Intn(len(APR1abc))]
	}
	return string(b)
}

// This is the MD5 hashing function out of Apache's htpasswd program. The algorithm
// is insane, but we have to match it. Mercifully I found a PHP variant of it at
//   http://stackoverflow.com/questions/2994637/how-to-edit-htpasswd-using-php
// in an answer. That reads better than the original C, and is easy to instrument.
// We will eventually go back to the original apr_md5.c for inspiration when the
// PHP gets too weird.
// The algorithm makes more sense if you imagine the original authors in a pub,
// drinking beer and rolling dice as the fundamental design process.
//
// This implementation has been copied from:
// https://github.com/tg123/go-htpasswd/blob/master/md5.go
// https://github.com/jimstudt/http-authentication/blob/master/basic/md5.go
func computeAPR1(password string, salt string) string {

	// start with a hash of password and salt
	initBin := md5.Sum([]byte(password + salt + password))

	// begin an initial string with hash and salt
	initText := bytes.NewBufferString(password + "$apr1$" + salt)

	// add crap to the string willy-nilly
	for i := len(password); i > 0; i -= 16 {
		lim := i
		if lim > 16 {
			lim = 16
		}
		initText.Write(initBin[0:lim])
	}

	// add more crap to the string willy-nilly
	for i := len(password); i > 0; i >>= 1 {
		if (i & 1) == 1 {
			initText.WriteByte(byte(0))
		} else {
			initText.WriteByte(password[0])
		}
	}

	// Begin our hashing in earnest using our initial string
	bin := md5.Sum(initText.Bytes())

	n := bytes.NewBuffer([]byte{})

	for i := 0; i < 1000; i++ {
		// prepare to make a new muddle
		n.Reset()

		// alternate password+crap+bin with bin+crap+password
		if (i & 1) == 1 {
			n.WriteString(password)
		} else {
			n.Write(bin[:])
		}

		// usually add the salt, but not always
		if i%3 != 0 {
			n.WriteString(salt)
		}

		// usually add the password but not always
		if i%7 != 0 {
			n.WriteString(password)
		}

		// the back half of that alternation
		if (i & 1) == 1 {
			n.Write(bin[:])
		} else {
			n.WriteString(password)
		}

		// replace bin with the md5 of this muddle
		bin = md5.Sum(n.Bytes())
	}

	// At this point we stop transliterating the PHP code and flip back to
	// reading the Apache source. The PHP uses their base64 library, but that
	// uses the wrong character set so needs to be repaired afterwards and reversed
	// and it is just really weird to read.

	result := bytes.NewBuffer([]byte{})

	// This is our own little similar-to-base64-but-not-quite filler
	fill := func(a byte, b byte, c byte) {
		v := (uint(a) << 16) + (uint(b) << 8) + uint(c) // take our 24 input bits

		for i := 0; i < 4; i++ { // and pump out a character for each 6 bits
			result.WriteByte(APR1abc[v&0x3f])
			v >>= 6
		}
	}

	// The order of these indices is strange, be careful
	fill(bin[0], bin[6], bin[12])
	fill(bin[1], bin[7], bin[13])
	fill(bin[2], bin[8], bin[14])
	fill(bin[3], bin[9], bin[15])
	fill(bin[4], bin[10], bin[5]) // 5?  Yes.
	fill(0, 0, bin[11])

	resultString := string(result.Bytes()[0:22]) // we wrote two extras since we only need 22.

	return fmt.Sprintf("$%s$%s$%s", "apr1", salt, resultString)
}