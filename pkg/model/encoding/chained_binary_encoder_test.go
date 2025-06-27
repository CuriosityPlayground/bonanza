package encoding_test

import (
	"testing"

	"bonanza.build/pkg/model/encoding"

	"github.com/stretchr/testify/require"

	"go.uber.org/mock/gomock"
)

func TestChainedBinaryEncoder(t *testing.T) {
	ctrl := gomock.NewController(t)

	t.Run("Zero", func(t *testing.T) {
		// If no encoders are provided, the resulting chained
		// encoder should act as the identity function.
		binaryEncoder := encoding.NewChainedBinaryEncoder(nil)

		t.Run("Encode", func(t *testing.T) {
			encodedData, decodingParameters, err := binaryEncoder.EncodeBinary([]byte("Hello"))
			require.NoError(t, err)
			require.Equal(t, []byte("Hello"), encodedData)
			require.Empty(t, decodingParameters)
		})

		t.Run("Decode", func(t *testing.T) {
			decodedData, err := binaryEncoder.DecodeBinary([]byte("Hello"), nil)
			require.NoError(t, err)
			require.Equal(t, []byte("Hello"), decodedData)
		})
	})

	t.Run("One", func(t *testing.T) {
		binaryEncoder1 := NewMockBinaryEncoder(ctrl)
		binaryEncoder := encoding.NewChainedBinaryEncoder([]encoding.BinaryEncoder{
			binaryEncoder1,
		})

		t.Run("Encode", func(t *testing.T) {
			binaryEncoder1.EXPECT().EncodeBinary([]byte("Hello")).
				Return([]byte("World"), []byte("Parameters"), nil)

			encodedData, decodingParameters, err := binaryEncoder.EncodeBinary([]byte("Hello"))
			require.NoError(t, err)
			require.Equal(t, []byte("World"), encodedData)
			require.Equal(t, []byte("Parameters"), decodingParameters)
		})

		t.Run("Decode", func(t *testing.T) {
			binaryEncoder1.EXPECT().DecodeBinary([]byte("World"), []byte("Parameters")).
				Return([]byte("Hello"), nil)

			decodedData, err := binaryEncoder.DecodeBinary([]byte("World"), []byte("Parameters"))
			require.NoError(t, err)
			require.Equal(t, []byte("Hello"), decodedData)
		})
	})

	t.Run("Two", func(t *testing.T) {
		binaryEncoder1 := NewMockBinaryEncoder(ctrl)
		binaryEncoder2 := NewMockBinaryEncoder(ctrl)
		binaryEncoder := encoding.NewChainedBinaryEncoder([]encoding.BinaryEncoder{
			binaryEncoder1,
			binaryEncoder2,
		})

		t.Run("Encode", func(t *testing.T) {
			// When encoding, the encoders should be applied
			// from first to last (e.g., compress and
			// encrypt).
			gomock.InOrder(
				binaryEncoder1.EXPECT().EncodeBinary([]byte("Foo")).
					Return([]byte("Bar"), nil, nil),
				binaryEncoder2.EXPECT().EncodeBinary([]byte("Bar")).
					Return([]byte("Baz"), []byte("Parameters"), nil),
			)

			encodedData, decodingParameters, err := binaryEncoder.EncodeBinary([]byte("Foo"))
			require.NoError(t, err)
			require.Equal(t, []byte("Baz"), encodedData)
			require.Equal(t, []byte("Parameters"), decodingParameters)
		})

		t.Run("Decode", func(t *testing.T) {
			// When decoding, the encoders should be applied
			// the other way around (e.g., decrypt and
			// decompress).
			gomock.InOrder(
				binaryEncoder2.EXPECT().DecodeBinary([]byte("Baz"), []byte("Parameters")).
					Return([]byte("Bar"), nil),
				binaryEncoder1.EXPECT().DecodeBinary([]byte("Bar"), nil).
					Return([]byte("Foo"), nil),
			)

			decodedData, err := binaryEncoder.DecodeBinary([]byte("Baz"), []byte("Parameters"))
			require.NoError(t, err)
			require.Equal(t, []byte("Foo"), decodedData)
		})
	})
}
