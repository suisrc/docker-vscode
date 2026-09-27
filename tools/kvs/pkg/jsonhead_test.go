package pkg

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestJSONHeadStringIs 覆盖快路径判定器：命中、未命中、转义、诱饵字符串、嵌套与
// 各种非法/边界输入都必须与"完整解析"的结论一致（不确定的一律 false → 回退完整解析）。
func TestJSONHeadStringIs(t *testing.T) {
	big := strings.Repeat("x", 1<<16)
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"type first", `{"type":"data","payload":"abc"}`, true},
		{"type last", `{"payload":"abc","type":"data"}`, true},
		{"whitespace", "{ \"type\" :\t\"data\"\n}", true},
		{"other type", `{"type":"pair_status_query"}`, false},
		{"other key", `{"kind":"data"}`, false},
		{"key prefix", `{"typex":"data"}`, false},
		{"nested only", `{"a":{"type":"data"}}`, false},
		{"top-level array", `[{"type":"data"}]`, false},
		{"empty object", `{}`, false},
		{"empty frame", ``, false},
		{"not json", `hello`, false},
		{"unterminated", `{"type":"data`, false},
		{"escaped value", `{"type":"da\u0074a"}`, true},
		{"decoy in string", `{"payload":"\"type\":\"data\"","type":"pair_status_query"}`, false},
		{"decoy after payload", `{"payload":"xx\"type\":\"data\"xx"}`, false},
		{"payload object first", `{"payload":{"a":[1,2,{"b":"}"}]},"type":"data","x":1}`, true},
		{"payload array first", `{"payload":[1,2,3],"type":"data"}`, true},
		{"payload number first", `{"payload":123,"type":"data"}`, true},
		{"payload null first", `{"payload":null,"type":"data"}`, true},
		{"payload true first", `{"payload":true,"type":"data"}`, true},
		{"payload escaped quote", `{"p":"a\"b","type":"data"}`, true},
		{"colon in string", `{"p":"a:b","type":"data"}`, true},
		{"comma in string", `{"p":"a,b","type":"data"}`, true},
		{"brace in string", `{"p":"a}b","type":"data"}`, true},
		{"big payload first", fmt.Sprintf(`{"payload":"%s","type":"data"}`, big), true},
	}
	for _, tc := range cases {
		if got := jsonHeadStringIs([]byte(tc.frame), "type", "data"); got != tc.want {
			t.Errorf("%s: jsonHeadStringIs(%q) = %v, want %v", tc.name, truncate(tc.frame), got, tc.want)
		}
	}
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// BenchmarkJSONHeadDataVsParse 对比 data 帧两条路径：头部扫描（本次新增）与完整解析。
func BenchmarkJSONHeadData(b *testing.B) {
	frame := []byte(`{"type":"data","payload":"` + strings.Repeat("A", 1<<20) + `"}`)
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !jsonHeadStringIs(frame, "type", "data") {
			b.Fatal("fast path missed")
		}
	}
}

func BenchmarkJSONHeadDataParse(b *testing.B) {
	frame := []byte(`{"type":"data","payload":"` + strings.Repeat("A", 1<<20) + `"}`)
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var msg zcodeClientMsg
		if err := json.Unmarshal(frame, &msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONHeadDataLateType 最坏情况：type 在 payload 之后（需要线性扫描）。
func BenchmarkJSONHeadDataLateType(b *testing.B) {
	frame := []byte(`{"payload":"` + strings.Repeat("A", 1<<20) + `","type":"data"}`)
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !jsonHeadStringIs(frame, "type", "data") {
			b.Fatal("fast path missed")
		}
	}
}
