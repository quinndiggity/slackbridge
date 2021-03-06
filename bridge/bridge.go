package bridge

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/matrix-org/slackbridge/common"
	"github.com/matrix-org/slackbridge/matrix"
	"github.com/matrix-org/slackbridge/slack"
)

type Config struct {
	MatrixASAccessToken string
	UserPrefix          string
	HomeserverBaseURL   string
	HomeserverName      string
}

type Bridge struct {
	UserMap              *UserMap
	RoomMap              *RoomMap
	SlackRoomMembers     *slack.RoomMembers
	MatrixUsers          *matrix.Users
	Client               http.Client
	MatrixEchoSuppresser *common.EchoSuppresser
	Config               Config
}

func (b *Bridge) OnSlackMessage(m slack.Message) {
	matrixRoom := b.RoomMap.MatrixForSlack(m.Channel)
	if matrixRoom == nil {
		log.Printf("Ignoring event for unknown slack room %q", m.Channel)
		return
	}
	matrixUser := b.UserMap.MatrixForSlack(m.User)
	if matrixUser == nil {
		matrixUser = b.matrixUserFor(m.Channel, m.User, matrixRoom)
	}
	if matrixUser == nil {
		log.Printf("Ignoring event from unknown slack user %q", m.User)
		return
	}

	if m.Subtype == "me_message" {
		if err := matrixUser.Client.SendEmote(matrixRoom.ID, slackToMatrix(m.Text)); err != nil {
			log.Printf("Error sending emote to Matrix: %v", err)
		}
		return
	}
	if m.File != nil {
		if handled := b.handleSlackFile(m, matrixRoom.ID, matrixUser); handled {
			return
		}
	}

	if err := matrixUser.Client.SendText(matrixRoom.ID, slackToMatrix(m.Text)); err != nil {
		log.Printf("Error sending text to Matrix: %v", err)
	}
}

func (b *Bridge) handleSlackFile(m slack.Message, matrixRoom string, matrixUser *matrix.User) bool {
	if !strings.HasPrefix(m.File.MIMEType, "image/") {
		return false
	}
	matrixImage := &matrix.Image{
		URL: m.File.URL,
		Info: &matrix.ImageInfo{
			Width:    m.File.OriginalWidth,
			Height:   m.File.OriginalHeight,
			MIMEType: m.File.MIMEType,
			Size:     m.File.Size,
		},
	}
	basename := path.Base(m.File.URL)
	if err := matrixUser.Client.SendImage(matrixRoom, basename, matrixImage); err != nil {
		log.Printf("Error sending image to Matrix: %v", err)
	}
	if m.File.CommentsCount == 1 && m.File.InitialComment != nil {
		if err := matrixUser.Client.SendText(matrixRoom, slackToMatrix(m.File.InitialComment.Comment)); err != nil {
			log.Printf("Error sending text to Matrix: %v", err)
		}
	}
	return true
}

func (b *Bridge) OnMatrixRoomMember(m matrix.RoomMemberEvent) {
	room := b.RoomMap.MatrixRoom(m.RoomID)
	if room == nil {
		log.Printf("Ignoring membership event for matrix room %q", m.RoomID)
		return
	}
	if m.Content.Membership == "join" {
		room.Users[m.UserID] = m.Content
	}
}

func (b *Bridge) OnMatrixRoomMessage(m matrix.RoomMessage) {
	slackChannel := b.RoomMap.SlackForMatrix(m.RoomID)
	if slackChannel == "" {
		log.Printf("Ignoring event for unknown matrix room %q", m.RoomID)
		return
	}
	slackUser := b.UserMap.SlackForMatrix(m.UserID)
	if slackUser == nil {
		slackUser = b.slackUserFor(slackChannel, m.UserID)
	}
	if slackUser == nil {
		log.Printf("Ignoring event from unknown matrix user %q", m.UserID)
		return
	}
	var c matrix.TextMessageContent
	if err := json.Unmarshal(m.Content, &c); err != nil {
		log.Printf("Error unmarshaling room message content: %v", err)
		return
	}
	if c.MsgType == "m.image" {
		if err := b.handleMatrixImage(m, slackChannel, slackUser); err == nil {
			return
		} else {
			log.Printf("Error sending image to slack: %v - falling back to text", err)
		}
	}

	if err := slackUser.Client.SendText(slackChannel, matrixToSlack(c.Body)); err != nil {
		log.Printf("Error sending text to Slack: %v", err)
	}
}

func (b *Bridge) handleMatrixImage(m matrix.RoomMessage, slackChannel string, slackUser *slack.User) error {
	var c matrix.ImageMessageContent
	if err := json.Unmarshal(m.Content, &c); err != nil {
		return fmt.Errorf("Error unmarshaling room message content: %v", err)
	}
	return slackUser.Client.SendImage(slackChannel, matrixToSlack(c.Body), b.mxcToHTTPS(c.URL))
}

func (b *Bridge) slackUserFor(slackChannel, matrixUserID string) *slack.User {
	token := b.botAccessToken(slackChannel)
	if token == "" {
		return nil
	}

	var iconURL string
	var displayName string
	matrixRoom := b.RoomMap.MatrixForSlack(slackChannel)
	if matrixRoom != nil {
		userInfo := matrixRoom.Users[matrixUserID]
		iconURL = b.mxcToHTTPS(userInfo.AvatarURL)
		displayName = userInfo.DisplayName
	}

	client := slack.NewBotClient(token, matrixUserID, displayName, iconURL, b.Client, b.RoomMap.ShouldNotify)
	user := &slack.User{matrixUserID, client}
	b.SlackRoomMembers.Add(slackChannel, user)
	return user
}

func (b *Bridge) botAccessToken(slackChannel string) string {
	user := b.SlackRoomMembers.Any(slackChannel)
	if user == nil {
		return ""
	}
	return user.Client.AccessToken()
}

func (b *Bridge) mxcToHTTPS(url string) string {
	if !strings.HasPrefix(url, "mxc://") {
		return url
	}
	return fmt.Sprintf("%s/_matrix/media/v1/download/%s", b.Config.HomeserverBaseURL, url[len("mxc://"):])
}

func (b *Bridge) matrixUserFor(slackChannel, slackUserID string, matrixRoom *matrix.Room) *matrix.User {
	slackUserInRoom := b.SlackRoomMembers.Any(slackChannel)
	if slackUserInRoom == nil {
		return nil
	}
	resp, err := b.Client.Get(fmt.Sprintf("https://slack.com/api/users.info?token=%s&user=%s", slackUserInRoom.Client.AccessToken(), slackUserID))
	if err != nil {
		log.Printf("Error looking up user %q: %v", slackUserID, err)
		return nil
	}
	defer resp.Body.Close()
	respBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading user info response: %v", err)
		return nil
	}
	var r slackUserInfoResponse
	if err := json.Unmarshal(respBytes, &r); err != nil {
		log.Printf("Error unmarshaling user info response: %v (%s)", err, string(respBytes))
		return nil
	}

	if r.User == nil {
		log.Printf("Ignoring slack message from non-slack user - probably our own Matrix bot")
		return nil
	}

	b.MatrixUsers.Mu.Lock()
	matrixUserID := b.Config.UserPrefix + r.User.Name + ":" + b.Config.HomeserverName
	user := b.MatrixUsers.Get_Locked(matrixUserID)
	if user == nil {
		client := matrix.NewBotClient(b.Config.MatrixASAccessToken, matrixUserID, b.Client, b.Config.HomeserverBaseURL, b.MatrixEchoSuppresser)
		user = matrix.NewUser(matrixUserID, client)
		b.MatrixUsers.Save_Locked(user)
	}
	b.MatrixUsers.Mu.Unlock()

	if !user.Rooms(false)[matrixRoom.ID] {
		if err := b.matrixBotClient().JoinRoom(matrixRoom.ID); err != nil {
			log.Printf("Error joining bot to room: %v", err)
		}
		if err := b.matrixBotClient().Invite(matrixRoom.ID, matrixUserID); err != nil {
			log.Printf("Error inviting to room: %v", err)
		}
		if err := user.JoinRoom(matrixRoom.ID); err != nil {
			log.Printf("Error joining room: %v", err)
			return nil
		}
	}
	return user
}

func (b *Bridge) matrixBotClient() matrix.Client {
	return matrix.NewClient(b.Config.MatrixASAccessToken, b.Client, b.Config.HomeserverBaseURL, b.MatrixEchoSuppresser)
}

type slackUserInfoResponse struct {
	OK   bool       `json:"ok"`
	User *slackUser `json:"user"`
}

type slackUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
