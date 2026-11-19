package s3test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"
)

// lookup in the given environment, as [os.LookupEnv] does in the process's.
func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// root of a module with the given files, by name and content.
func root(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		is.NotError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

const envTest = `# A comment, with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in it.
S4QL_S3_BUCKET=bucket
AWS_ENDPOINT_URL_S3=https://store.example
AWS_REGION=auto
`

func TestLoad(t *testing.T) {
	t.Run("should take the settings from .env.test and the credentials from .env", func(t *testing.T) {
		dir := root(t, map[string]string{
			".env.test": envTest,
			".env":      "AWS_ACCESS_KEY_ID=id\nexport AWS_SECRET_ACCESS_KEY='secret'\nOTHER=x\n",
		})
		c, err := load(lookup(nil), dir)
		is.NotError(t, err)
		is.Equal(t, Config{Bucket: "bucket", Endpoint: "https://store.example", Region: "auto", AccessKeyID: "id", SecretAccessKey: "secret"}, c)
	})

	t.Run("should take a name that the environment sets from the environment, over both files", func(t *testing.T) {
		dir := root(t, map[string]string{
			".env.test": envTest + "S4QL_S3_PATH_STYLE=false\n",
			".env":      "AWS_ACCESS_KEY_ID=id\nAWS_SECRET_ACCESS_KEY=secret\n",
		})
		c, err := load(lookup(map[string]string{
			"S4QL_S3_BUCKET": "other", "AWS_REGION": "", "S4QL_S3_PATH_STYLE": "true",
			"AWS_ACCESS_KEY_ID": "envid", "AWS_SECRET_ACCESS_KEY": "envsecret",
		}), dir)
		is.NotError(t, err)
		// An empty variable is taken as not set.
		is.Equal(t, Config{
			Bucket: "other", Endpoint: "https://store.example", Region: "auto", PathStyle: true,
			AccessKeyID: "envid", SecretAccessKey: "envsecret",
		}, c)
	})

	t.Run("should take only the settings from .env.test and only the credentials from .env", func(t *testing.T) {
		dir := root(t, map[string]string{
			".env.test": "AWS_ACCESS_KEY_ID=id\nAWS_SECRET_ACCESS_KEY=secret\n",
			".env":      "S4QL_S3_BUCKET=bucket\n",
		})
		_, err := load(lookup(nil), dir)
		is.True(t, err != nil, "no error")
		is.True(t, strings.Contains(err.Error(), "S4QL_S3_BUCKET is set neither in the environment nor in "+filepath.Join(dir, ".env.test")), err.Error())
		is.True(t, strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are set neither in the environment nor in "+filepath.Join(dir, ".env")), err.Error())
	})

	t.Run("should fail with what is missing and how to skip, without .env and without credentials in the environment", func(t *testing.T) {
		dir := root(t, map[string]string{".env.test": envTest})
		_, err := load(lookup(nil), dir)
		is.True(t, err != nil, "no error")
		is.Equal(t, "s3test: the tests against a real bucket cannot run: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are set neither in the environment nor in "+
			filepath.Join(dir, ".env")+"; run go test with -short to skip these tests, and see the documentation of internal/store/s3test", err.Error())
	})

	t.Run("should take the credentials as a pair, from the environment if it sets one of them", func(t *testing.T) {
		dir := root(t, map[string]string{".env.test": envTest, ".env": "AWS_ACCESS_KEY_ID=id\nAWS_SECRET_ACCESS_KEY=secret\n"})
		_, err := load(lookup(map[string]string{"AWS_ACCESS_KEY_ID": "envid"}), dir)
		is.True(t, err != nil, "no error")
		is.True(t, strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY is not set in the environment, which sets AWS_ACCESS_KEY_ID"), err.Error())
		is.True(t, !strings.Contains(err.Error(), "envid"), "the error shows a value")

		dir = root(t, map[string]string{".env.test": envTest, ".env": "AWS_SECRET_ACCESS_KEY=secret\n"})
		_, err = load(lookup(nil), dir)
		is.True(t, err != nil, "no error")
		is.True(t, strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID is not set in "+filepath.Join(dir, ".env")+", which sets AWS_SECRET_ACCESS_KEY"), err.Error())
		is.True(t, !strings.Contains(err.Error(), "secret\n"), "the error shows a value")
	})

	t.Run("should strip matching quotes and an export, and take the last of a name that is given twice", func(t *testing.T) {
		dir := root(t, map[string]string{
			".env.test": "S4QL_S3_BUCKET=first\n  export S4QL_S3_BUCKET = \"bucket\"  \nAWS_REGION='a\nAWS_ENDPOINT_URL_S3=\n",
			".env":      "AWS_ACCESS_KEY_ID=\"id\"\r\nAWS_SECRET_ACCESS_KEY=se=cret\r\n",
		})
		c, err := load(lookup(nil), dir)
		is.NotError(t, err)
		is.Equal(t, Config{Bucket: "bucket", Region: "'a", AccessKeyID: "id", SecretAccessKey: "se=cret"}, c)
	})

	t.Run("should drop a comment after an unquoted value, and take export with any space after it", func(t *testing.T) {
		dir := root(t, map[string]string{
			".env.test": "S4QL_S3_BUCKET=bucket # The bucket.\nAWS_REGION=a#b\nAWS_ENDPOINT_URL_S3=\"https://x # y\"\n",
			".env":      "export\tAWS_ACCESS_KEY_ID=id\t# The key.\nexport  AWS_SECRET_ACCESS_KEY=secret\n",
		})
		c, err := load(lookup(nil), dir)
		is.NotError(t, err)
		is.Equal(t, Config{Bucket: "bucket", Region: "a#b", Endpoint: "https://x # y", AccessKeyID: "id", SecretAccessKey: "secret"}, c)
	})

	t.Run("should fail on a file that it cannot read, other than a missing one", func(t *testing.T) {
		dir := root(t, map[string]string{".env.test": envTest})
		is.NotError(t, os.Mkdir(filepath.Join(dir, ".env"), 0o700))
		_, err := load(lookup(nil), dir)
		is.True(t, err != nil && strings.Contains(err.Error(), filepath.Join(dir, ".env")), err)
	})
}

func TestModuleRoot(t *testing.T) {
	t.Run("should find the root of the module maragu.dev/s4ql above the working directory", func(t *testing.T) {
		dir, err := moduleRoot()
		is.NotError(t, err)
		_, err = os.Stat(filepath.Join(dir, ".env.test"))
		is.NotError(t, err)
		wd, err := os.Getwd()
		is.NotError(t, err)
		is.Equal(t, filepath.Join(dir, "internal", "store", "s3test"), wd)
	})

	t.Run("should pass over the go.mod of another module", func(t *testing.T) {
		dir := root(t, map[string]string{"go.mod": "// The module.\nmodule maragu.dev/s4ql\n\ngo 1.27\n"})
		inner := filepath.Join(dir, "a", "b")
		is.NotError(t, os.MkdirAll(inner, 0o700))
		is.NotError(t, os.WriteFile(filepath.Join(dir, "a", "go.mod"), []byte("module maragu.dev/s4ql/a\n"), 0o600))
		t.Chdir(inner)
		got, err := moduleRoot()
		is.NotError(t, err)
		want, err := filepath.EvalSymlinks(dir)
		is.NotError(t, err)
		got, err = filepath.EvalSymlinks(got)
		is.NotError(t, err)
		is.Equal(t, want, got)
	})

	t.Run("should fail outside the module", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := moduleRoot()
		is.True(t, err != nil, "no error")
	})
}

func TestOnce(t *testing.T) {
	t.Run("should run until it succeeds, and then not again", func(t *testing.T) {
		var o once
		var runs int
		fail := errors.New("fail")
		is.Error(t, fail, o.do(func() error { runs++; return fail }))
		is.NotError(t, o.do(func() error { runs++; return nil }))
		is.NotError(t, o.do(func() error { runs++; return fail }))
		is.Equal(t, 2, runs)
	})
}
