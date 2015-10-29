package matrix

type Client interface {
	SendText(roomID, text string) error
	SendImage(roomID, text string, image *Image) error
	SendEmote(matrixRoom, emote string) error
	JoinRoom(roomID string) error
	ListRooms() (map[string]bool, error)
	Invite(roomID, userID string) error

	Homeserver() string
	AccessToken() string
}

type Image struct {
	URL  string
	Info *ImageInfo
}
