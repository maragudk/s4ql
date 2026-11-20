# The plans of the queries on the schema readme, which TestAccessOf_golden in access_test.go checks.
# Rewrite with: go test ./internal/engine -run TestAccessOf_golden -update

create table t (k int primary key, a int not null, b text, c real) strict, without rowid
create index ia on t (a)
create index iab on t (a, b)

## a
select * from t where a = 10
access: index ia (a, k), pins a = 10

## a and b
select * from t where a = 10 and b = 'x'
access: index iab (a, b, k), pins a = 10, b = 'x'

## a and a range of b
select * from t where a = 10 and b > 'x'
access: index iab (a, b, k), pins a = 10, bounds 'x' < b

## a and a range of k
select * from t where a = 10 and k < 2
access: index ia (a, k), pins a = 10, bounds k < 2

## b
select * from t where b = 'x'
access: every row

## k and a
select * from t where k = 2 and a = 10
access: get, k = 2
